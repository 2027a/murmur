# Sinks

La livraison finale implémente six modes:

| Mode | Effet |
|---|---|
| `paste` | copie le texte selon la politique clipboard, puis émet le premier raccourci configuré ou frappe directement si la liste est vide |
| `type` | transmet le texte à `wtype -` |
| `clipboard` | conserve le texte final dans un presse-papiers persistant |
| `stdout` | écrit le texte suivi d'un retour à la ligne |
| `file` | remplace atomiquement le chemin explicite, avec des permissions `0600` |
| `auto` | devient la valeur explicite de `sink.auto_mode` (`paste` ou `type`) pendant la résolution de configuration |

`sink.New` retourne le sink composé directement. L'orchestrateur possède le
garde de livraison concurrency-safe au plus une fois; tous les appels concurrents
observent le résultat du premier appel effectif.

## Presse-papiers

`internal/clipboard` exécute `wl-copy` sans shell:

- `raw-and-clean`: après processing réussi et fermeture de l'annulation,
  `CopyRaw` copie le transcript brut, puis `CopyFinal` le remplace par le texte
  final;
- `clean-only`: seul `CopyFinal` écrit le texte;
- `none`: aucune commande clipboard n'est exécutée;
- `transient`: le texte final est copié pour la livraison, puis
  `wl-copy --clear` est tenté après les sinks supportés, y compris si ceux-ci
  échouent.

L'effacement transient reste une tentative: il ne garantit pas la suppression
des copies déjà capturées par un gestionnaire d'historique. Son échec est
cependant observable et fait échouer la livraison en `sink_failed`. Un clipboard
persistant n'est jamais effacé sur erreur: un échec de `CopyFinal` en
`raw-and-clean` peut donc laisser le brut courant, et un échec du sink après la
copie finale laisse le final courant. Aucun de ces comportements ne prétend
retirer les valeurs déjà capturées par un historique. `none` ne copie rien et
`transient` tente son effacement exactement une fois avant de rendre le résultat,
y compris si `CopyFinal` échoue; les erreurs de copie, sink et clear sont alors
jointes. Si la deadline de livraison expire pendant l'injection, l'effacement
reçoit un nouveau budget borné de cinq secondes afin que le timeout du sink ne
neutralise pas la transaction. Une annulation explicite reste prioritaire et
n'est pas détachée.

Un mode effectif `paste` avec au moins un raccourci et
`clipboard_mode="transient"` est refusé au preflight, avant capture. Le succès de
`wtype` confirme l'émission du raccourci, jamais la lecture de la sélection par
l'application cible; effacer immédiatement créerait donc une course sans
mécanisme de confirmation fiable. Une liste `paste_shortcuts` vide ne colle pas:
elle passe directement par `wtype -` et reste supportée, comme `type`.

`transient` ne constitue dans aucun mode une garantie de confidentialité: un
gestionnaire d'historique peut copier la valeur avant le clear, lequel peut aussi
échouer. Utiliser `clean-only` si un collage fiable et persistant prime, `type`
avec `transient` si cet effacement best-effort suffit, ou `type` avec `none` si
le texte ne doit jamais entrer dans le presse-papiers. `paste` avec `none` suit
également ce dernier chemin de frappe directe.

Le mode de session `clipboard-only` est résolu avant la construction de tout
injecteur et remplace donc une configuration globale `paste`, `type` ou `auto`.
Il refuse `none` et `transient` avant capture. Le sink explicite `clipboard`
exige lui aussi `raw-and-clean` ou `clean-only`; cette combinaison est refusée
par le préflight parent, puis contrôlée à nouveau par `sink.New`.

L'orchestrateur exécute d'abord tout le processing sans effet clipboard. Une
annulation acceptée, un résultat vide (`no_speech`) ou une erreur de processing
n'appelle donc ni `CopyRaw` ni `CopyFinal`. La transition atomique vers
`delivering` ferme ensuite la fenêtre d'annulation avant `CopyRaw`; `CopyFinal`
suit dans la même phase. Les deux appels suivent la même politique et le même
sink effectif, y compris en `clipboard-only`; le texte brut n'est ajouté ni aux
événements ni aux logs. Tout échec de copie termine en `failed/sink_failed`.

## Injection

Le texte issu des providers n'est pas fiable. La frontière d'injection active
est appliquée lorsque le mode effectif est `paste` ou `type`, y compris après
résolution de `auto`, avant tout `wl-copy` destiné à ce collage et avant tout
`wtype -`. Elle couvre aussi la copie brute `raw-and-clean` d'une telle session,
qui reçoit donc exactement la même forme du texte.

Cette frontière ne modifie pas les sinks passifs `clipboard`, `stdout` et
`file`, leurs éventuelles copies annexes, ni une session `clipboard-only`. Ces
sinks conservent leur contrat de transport du texte provider tel quel;
`stdout` ajoute seulement son retour à la ligne final historique.

Les raccourcis `wtype` supportés utilisent exactement les arguments suivants:

```text
ctrl+shift+v  -> wtype -M ctrl -M shift v -m shift -m ctrl
shift+insert  -> wtype -M shift Insert -m shift
ctrl+v        -> wtype -M ctrl v -m ctrl
```

Toute autre chaîne dans `paste_shortcuts` est une erreur de configuration avant
fork ou construction de l'injecteur. `inject.New` répète ce contrôle pour les
appels directs.

`paste` n'émet qu'une seule tentative active: le premier raccourci configuré. Ce
succès signifie seulement que la commande a été émise, pas que l'application
cible a accepté le collage. Si cette émission échoue techniquement, la livraison
échoue; la frappe directe `wtype -` n'est utilisée que lorsque `paste_shortcuts`
est vide.

`type_newlines` applique l'un des rendus suivants à toute injection active:

| Mode | Rendu des retours à la ligne |
|---|---|
| `space` | remplacés par une espace; défaut sûr |
| `marker` | remplacés par ` [NL] ` |
| `dangerous-keep` | normalisés en LF et conservés; opt-in dangereux pouvant produire une validation dans l'application focalisée |

CRLF et CR sont d'abord normalisés en LF. Une tabulation devient une espace
ASCII. Les séquences UTF-8 invalides deviennent U+FFFD, tous les caractères de
contrôle Unicode de catégorie `Cc` sont retirés, C0, DEL, ESC et C1 compris, et
les autres scalaires Unicode sont préservés. En `dangerous-keep`, LF constitue
la seule exception à ce retrait; les autres contrôles ne redeviennent jamais
injectables. Les séparateurs Unicode tels que U+2028 restent des caractères
Unicode et ne sont pas convertis en touche Enter.

L'ancien mode `keep` n'est pas un alias: il est rejeté au preflight. Une
configuration qui veut réellement injecter des retours à la ligne doit donc
nommer explicitement `dangerous-keep`.

Avec une politique clipboard `none`, le sink `paste` passe directement par la
frappe afin de ne jamais coller un ancien contenu du presse-papiers.

## Budget de livraison

Chaque opération externe de livraison dispose d'un budget fixe de 5 secondes:
`CopyRaw`, `CopyFinal`, une tentative de collage, la frappe et `wl-copy --clear`.
Une deadline appelante plus courte reste prioritaire pour l'effet principal; seul
le clear transient compensatoire décrit plus haut peut recevoir son propre
budget après son expiration.

Après processing, un collage persistant est donc borné à 10 secondes en
`clean-only` (copie puis injection) et à 15 secondes en `raw-and-clean` (brut,
final, puis injection). Une frappe avec clipboard transient reste bornée à 15
secondes (copie, injection puis clear). Chaque copie possède son propre budget.
La commande foreground `dictate` applique également cette borne transactionnelle
de 15 secondes, sans réduire les trois budgets internes à une deadline commune
de cinq secondes.
Cette borne de sûreté est une constante interne, pas un réglage TOML: le schéma
`sink` décrit la politique de livraison, pas la durée pendant laquelle une
commande desktop défaillante peut retenir une session.

À expiration, `sysexec` envoie `SIGKILL` au groupe de processus de `wl-copy` ou
`wtype` et attend sa récolte. La session termine en `failed/sink_failed`, puis
libère son socket et son verrou; le démarrage suivant ne dépend donc pas du
helper défaillant.

Les sinks `stdout` et `file` associés à `clipboard_mode = "none"` ne
construisent ni n'exécutent `wl-copy` ou `wtype`; ils fonctionnent donc sans UI,
clipboard ou intégration au compositor.
