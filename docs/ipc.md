# Protocole IPC v1

L'IPC relie les invocations CLI au processus de session déjà lancé. Il n'est ni
exposé sur le réseau ni utilisé pour démarrer une session: `murmur start` prend
le verrou puis ré-exécute le binaire avec des descripteurs hérités.

## Transport et permissions

Le serveur écoute sur `$XDG_RUNTIME_DIR/murmur/session.sock`, dans un répertoire
`0700`; le socket est forcé à `0600`. Une connexion transporte une requête JSON
suivie d'un saut de ligne et, sauf pour `subscribe`, une réponse JSON suivie d'un
saut de ligne. Les champs inconnus, le JSON invalide et les versions inconnues
sont refusés.

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

Types possibles: `session_started`, `capture_ready`, `state_changed`,
`partial_started`, `partial_done`, `partial_failed`, `provider_selected`,
`degraded`, `delivered`, `terminal`. Les champs optionnels sont `state`,
`result`, `provider`, `error_class`, `component`, `partial` et `total`. La
structure exclut volontairement tout champ de texte libre ou transcript.

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

Le même `events.Bus` alimente IPC et UI. Les deux représentations partagent donc
les états et la provenance, tandis que leur cadrage reste distinct: événement
complet pour IPC, payload réduit pour l'OSD. Voir
[`osd-protocol.md`](osd-protocol.md).
