# Protocole IPC v1

L'IPC relie les invocations CLI au processus de session déjà lancé. Il n'est ni
exposé sur le réseau ni utilisé pour démarrer une session: `murmur start` prend
le verrou puis ré-exécute le binaire avec des descripteurs hérités.

## Transport et permissions

Le serveur et toutes les commandes clientes résolvent le même chemin déterministe:
`$XDG_RUNTIME_DIR/murmur/session.sock`, ou `/run/user/<euid>/murmur/session.sock`
si la variable est absente. Ce second parent doit déjà exister, être réel,
appartenir à l'EUID et être en `0700`; Murmur échoue plutôt que de se replier
sous `/tmp`. Les composants et le sous-répertoire `murmur` sont ouverts ou créés
avec des opérations ancrées à des descripteurs et sans suivre de symlink. Le
répertoire applicatif est en `0700`; le socket est forcé à `0600`. Une connexion
transporte exactement une requête JSON sur une ligne d'au plus 4 Kio, LF compris,
et, sauf pour `subscribe`, une réponse JSON suivie d'un saut de ligne. Le LF est
obligatoire; le serveur n'attend pas la fermeture de la moitié écriture. Un EOF
prématuré ou une ligne trop longue vaut `config_invalid`. Après l'unique valeur
JSON, seuls les blancs JSON sont admis avant le LF. Les champs inconnus, le JSON
invalide, une seconde valeur et les versions inconnues sont refusés sans appeler
le gestionnaire.

Les lectures et écritures du handshake sont bornées à deux secondes par défaut.
Le client classe un socket absent ou refusé comme `ipc.ErrNoSession`
(`unavailable`). Une requête invalide ferme seulement sa connexion et ne termine
pas le serveur.

## Requêtes et réponses

```json
{"v":1,"cmd":"stop"}
```

Commandes acceptées:

| Commande | Effet |
|---|---|
| `stop` | finalise la capture puis lance STT, pipeline et livraison |
| `cancel` | annule tant que la livraison n'a pas commencé |
| `toggle` | arrête en `starting`/`recording`, refusé dans les autres états actifs |
| `status` | renvoie l'état courant de la session active |
| `subscribe` | transforme la connexion en flux d'événements |

`start` n'est pas une commande IPC. La CLI publique n'expose pas non plus de
sous-commande `subscribe`; ce contrat sert aux intégrations qui utilisent
`internal/ipc.Client` ou parlent directement au socket local.

Une réponse réussie contient la version, `ok=true` et l'état résultant:

```json
{"v":1,"ok":true,"state":"stopping"}
```

Une réponse en erreur contient une classe fermée, un composant et un message
contrôlé:

```json
{"v":1,"ok":false,"error":{"class":"config_invalid","component":"ipc","message":"version IPC non supportée: 9"}}
```

Le protocole ne transporte ni configuration, ni secret, ni transcript. Au repos,
`murmur status` ne consulte pas le socket: il lit `last-result.json` et construit
sa propre réponse humaine ou JSON. Ce repli ne fabrique jamais une session
active.

## Abonnement

`subscribe` reçoit d'abord l'accusé standard:

```json
{"v":1,"ok":true,"state":"subscribed"}
```

Le délai de lecture est alors supprimé et le serveur écrit des événements JSONL
v1 jusqu'à l'annulation, la fermeture du serveur, la disparition du client ou
une erreur d'écriture:

```json
{"v":1,"type":"state_changed","timestamp":"2026-07-29T12:34:56Z","session_id":"session-1","state":"transcribing"}
```

La connexion reste néanmoins à requête unique: tout octet envoyé par le client
après le LF de `subscribe` ferme le flux et n'est jamais interprété comme une
seconde requête.

Types possibles: `session_started`, `capture_ready`, `state_changed`,
`partial_started`, `partial_done`, `partial_failed`, `provider_selected`,
`degraded`, `delivered`, `terminal`, `timing`. Les champs optionnels sont `state`,
`result`, `provider`, `error_class`, `component`, `partial`, `total` et `timing`. La
structure exclut volontairement tout champ de texte libre ou transcript.
Pour une transcription multi-provider, les événements globaux utilisent
`provider="mixed"`; les événements partiels gardent le provider du segment.
`partial` est l'index original du segment converti en base 1. Chaque tentative
provider réellement lancée publie `partial_started`, puis `partial_done` ou
`partial_failed` avec le même index et le même provider. Un fallback constitue
une nouvelle tentative; les segments non tentés après arrêt anticipé, annulation
ou dépassement de budget ne publient aucun `partial_started`. `total` est connu
après capture et reste omis pendant la capture live.

`timing` contient `stage`, `duration_ms` et, pour `stage="stt"`, les champs
optionnels `queue_ms` et `audio_ms`. Les étapes autorisées sont `capture_ready`,
`stt`, `stt_complete`, `cleanup`, `processing`, `delivery` et `after_stop`.
Les mesures STT portent le provider et l'index de la tentative. Les durées sont
entières, non négatives, et mesurées avec l'horloge monotone du processus pour les
opérations. Aucun texte libre n'est ajouté. Voir [latency.md](latency.md) pour
les bornes exactes de chaque mesure. Les timings n'alimentent pas l'UI.

Le bus et chaque abonnement sont bornés et non bloquants. Un client lent peut
perdre des événements; il doit reconstruire l'état via `status` plutôt que
supposer un journal exhaustif.

## Cycle de vie

`__session` adopte le verrou et décode le snapshot de configuration hérités sans
relire TOML ni environnement. Il entre dans `starting`, supprime un socket
obsolète et appelle `ipc.Listen` avant de résoudre les secrets ou construire les
providers. Le pipe de bootstrap annonce le succès uniquement après le `bind` et
le `chmod 0600`; `start` peut alors retourner sans course avec `status`, `stop`,
`toggle` ou `cancel`.

Pendant la construction, le handler de bootstrap relaie les commandes vers la
même machine d'état et le même contexte que l'orchestrateur. Le socket reste
disponible pendant le traitement après `stop`, jusqu'à la persistance du résultat
terminal. Il est ensuite fermé et supprimé avant la libération du verrou. Une
erreur de construction suit le même ordre: événement et résultat terminal, puis
fermeture du socket.

Un `Server` est lié à l'unique listener créé par `ipc.Listen` et ne peut pas être
resservi après son arrêt. `Close` est idempotent: il ferme le listener et toutes
les connexions actives, annule le contexte dérivé transmis aux handlers, puis
laisse `Serve` attendre leur terminaison. Un abonnement client observe alors
`EOF` sans que l'appelant doive aussi annuler le contexte fourni à `Serve`.

Le même `events.Bus` alimente IPC et UI. Les deux représentations partagent donc
les états et la provenance, tandis que leur cadrage reste distinct: événement
complet pour IPC, payload réduit pour l'OSD. Voir
[`osd-protocol.md`](osd-protocol.md).

Le frontend QuickShell reçoit des instantanés dérivés des mêmes événements via
un pont distinct, `murmur ui` et `ui.sock`, décrit dans
[`quickshell.md`](quickshell.md). Ses mesures acoustiques à 20 Hz ne passent pas
dans le bus ni le journal de session. Le pont appartient au shell et reste
disponible entre les dictées; `session.sock` garde son cycle de vie par session.
