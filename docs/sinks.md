# Sinks

La livraison finale implémente six modes:

| Mode | Effet |
|---|---|
| `paste` | copie le texte selon la politique clipboard, essaie les raccourcis configurés, puis retombe sur la frappe si toutes les commandes échouent |
| `type` | transmet le texte à `wtype -` |
| `clipboard` | conserve le texte final dans un presse-papiers persistant |
| `stdout` | écrit le texte suivi d'un retour à la ligne |
| `file` | remplace atomiquement le chemin explicite, avec des permissions `0600` |
| `auto` | devient la valeur explicite de `sink.auto_mode` (`paste` ou `type`) pendant la résolution de configuration |

Chaque sink construit par `sink.New` est enveloppé par une livraison
concurrency-safe au plus une fois. Tous les appels concurrents observent le
résultat du premier appel effectif.

## Presse-papiers

`internal/clipboard` exécute `wl-copy` sans shell:

- `raw-and-clean`: `CopyRaw` copie le transcript brut, puis `CopyFinal` le
  remplace par le texte final;
- `clean-only`: seul `CopyFinal` écrit le texte;
- `none`: aucune commande clipboard n'est exécutée;
- `transient`: le texte final est copié pour la livraison, puis
  `wl-copy --clear` est tenté après le sink, y compris si celui-ci échoue.

L'effacement transient est best-effort. Il ne garantit pas la suppression des
copies déjà capturées par un gestionnaire d'historique. Une erreur de sink
laisse le texte final disponible avec `raw-and-clean` ou `clean-only`; `none` ne
le copie pas et `transient` tente de l'effacer.

Le mode de session `clipboard-only` est résolu avant la construction de tout
injecteur et remplace donc une configuration globale `paste`, `type` ou `auto`.
Il refuse `none` et `transient` avant capture. Le sink explicite `clipboard`
exige lui aussi `raw-and-clean` ou `clean-only`.

`CopyRaw` est appelé par l'orchestrateur juste après l'assemblage STT et avant le
cleanup. Il suit la même politique et le même sink effectif, y compris en
`clipboard-only`; le texte brut n'est ajouté ni aux événements ni aux logs.

## Injection

La cascade `wtype` utilise exactement les arguments suivants:

```text
ctrl+shift+v  -> wtype -M ctrl -M shift v -m shift -m ctrl
shift+insert  -> wtype -M shift Insert -m shift
ctrl+v        -> wtype -M ctrl v -m ctrl
```

La cascade s'arrête au premier processus `wtype` réussi. Ce succès signifie
seulement que la commande a été émise, pas que l'application cible a accepté le
collage. Si toutes les émissions échouent techniquement, le texte est transmis
à `wtype -` sur stdin.

Pour la frappe, `type_newlines` applique l'un des rendus suivants:

| Mode | Rendu des retours à la ligne |
|---|---|
| `keep` | conservés tels quels |
| `space` | remplacés par une espace |
| `marker` | remplacés par ` [NL] ` |

Avec une politique clipboard `none`, le sink `paste` passe directement par la
frappe afin de ne jamais coller un ancien contenu du presse-papiers.

Les sinks `stdout` et `file` associés à `clipboard_mode = "none"` ne
construisent ni n'exécutent `wl-copy` ou `wtype`; ils fonctionnent donc sans UI,
clipboard ou intégration au compositor.
