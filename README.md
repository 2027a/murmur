# Murmur

Murmur est un outil de dictée push-to-talk pour Wayland. Un processus de session
capture un WAV canonique avec PipeWire, l'envoie à un provider STT compatible
OpenAI, applique un pipeline texte déterministe, puis livre le résultat par
collage, frappe, presse-papiers, stdout ou fichier. Le réseau autorisé, les
secrets, la rétention et l'interface sont explicites dans la configuration.

Dépôt source : <https://git.2027a.net/2027a/murmur>.

## Installation

La construction exige Go 1.23 ou ultérieur. L'installation utilise les chemins
GNU usuels, accepte `PREFIX` et `DESTDIR`, et n'installe aucune configuration
utilisateur:

```sh
make lint test build
make install PREFIX=/usr/local
```

Par défaut, les fichiers installés sont:

```text
/usr/local/bin/murmur
/usr/local/share/doc/murmur/README.md
/usr/local/share/doc/murmur/LICENSE
/usr/local/share/doc/murmur/docs/*
/usr/local/share/doc/murmur/examples/*
```

Le helper OSD GTK est optionnel. Il est installé sous le nom `murmur-osd`, dans
le même `BINDIR` que le binaire afin que Murmur et `doctor` le trouvent via
`PATH`:

```sh
make install PREFIX=/usr/local INSTALL_OSD=1
```

Pour construire un paquet sans écrire dans le système:

```sh
make install DESTDIR="$PWD/pkg" PREFIX=/usr INSTALL_OSD=1
```

Les variables d'installation et leur valeur par défaut sont:

| Variable | Défaut | Usage |
|---|---|---|
| `PREFIX` | `/usr/local` | préfixe logique |
| `DESTDIR` | vide | racine de staging, à répéter à la désinstallation |
| `BINDIR` | `$(PREFIX)/bin` | destination des exécutables |
| `DATADIR` | `$(PREFIX)/share` | destination des données |
| `DOCDIR` | `$(DATADIR)/doc/murmur` | destination de la documentation |
| `INSTALL_OSD` | `0` | installe le helper avec `1`; doit aussi valoir `1` pour le désinstaller |

`make installcheck` effectue installation, exécution du binaire installé et
désinstallation dans un `DESTDIR` temporaire. Les fichiers installés sont
autonomes: le binaire embarque les packs de commandes et le helper OSD est un
script complet; aucun chemin vers le checkout n'est requis à l'exécution.

## Dépendances

| Usage | Dépendances d'exécution |
|---|---|
| toute capture | PipeWire et `pw-record` |
| source PipeWire explicite vérifiée par `doctor` | `pw-cli` |
| presse-papiers | `wl-copy` (`wl-clipboard`) |
| sinks `paste` ou `type` | `wtype`; `paste` utilise aussi `wl-copy` sauf avec `clipboard_mode="none"` |
| UI `notify` | bus D-Bus de session et service `org.freedesktop.Notifications` |
| UI `osd` | helper optionnel `murmur-osd`, Python 3, PyGObject GTK3 et gtk-layer-shell |
| `ui.sound=true` | `pw-play`, `paplay` ou `canberra-gtk-play`, plus le son freedesktop par défaut |
| build | Go 1.23 ou ultérieur |

Un provider HTTP compatible OpenAI est naturellement nécessaire pour le STT.
Les commandes `transcribe` et `cleanup` n'exigent ni PipeWire ni intégration
desktop; `dictate` exige seulement `pw-record`. Son sink vaut `stdout` par défaut;
`file` doit être sélectionné explicitement et exige un chemin `sink.file`.

## Configuration

Murmur charge `$XDG_CONFIG_HOME/murmur/config.toml`, ou
`~/.config/murmur/config.toml`. En l'absence de fichier, les valeurs compilées
forment cette configuration locale minimale:

```toml
[stt]
provider = "local"

[stt.providers.local]
type = "openai-compatible"
base_url = "http://127.0.0.1:8000/v1"
model = "large-v3"
language = "fr"
timeout_s = 120
zone = "local"
```

Le fichier existant doit être régulier, appartenir à l'utilisateur effectif et
ne pas être modifiable par le groupe ou les autres (`0600` et `0644` sont
acceptés). Les liens symboliques finaux et intermédiaires sont acceptés afin de
permettre un répertoire de dotfiles. Murmur copie au plus 1 Mio depuis le
descripteur validé, contrôle ensuite device/inode/taille/mtime/ctime, puis rouvre
le chemin et compare l'inode. Réécrire in-place, remplacer la cible ou retargeter
un symlink pendant le chargement échoue avant interprétation, helper de
credential, capture ou réseau.

Le profil implicite est `local` (`offline` est synonyme): STT, cleanup et plugins restent sur la machine,
les WAV et diagnostics ne sont pas retenus. Les autres profils de données sont
`lan`, `lan-cloud-cleanup`, `cloud` et `cloud-no-history`. Ils fournissent des
défauts de zones et de rétention sans écraser une valeur TOML, environnement ou
override explicitement définie. `cloud-no-history` désactive aussi le clipboard
par défaut, sans prétendre effacer l'historique d'un gestionnaire externe ou la
rétention du provider distant.

Pour partir de l'exemple installé avec le préfixe par défaut:

```sh
mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/murmur"
install -m 0600 /usr/local/share/doc/murmur/examples/config.minimal.toml \
  "${XDG_CONFIG_HOME:-$HOME/.config}/murmur/config.toml"
```

`examples/config.full.toml` montre tous les groupes utiles: source audio,
provider LAN, fallback cloud, cleanup local, chunking, plugin, policy, sinks et
UI. Le schéma exhaustif, la précédence TOML/environnement et la validation sont
dans [`docs/configuration.md`](docs/configuration.md). Les clés inconnues sont
refusées. Le même préflight déterministe sert à `start`, aux commandes
composables et à `doctor`: il s'exécute avant fork, socket, credential helper,
capture ou réseau. Il exige notamment des modèles non vides, des raccourcis de
collage connus et un clipboard persistant pour le sink `clipboard`.

Toutes les durées configurables sont bornées à 24 h avant conversion; le backoff
reste limité à 60 s. Les plafonds de ressources sont 64 Mio de texte STT, 65536
segments, 16 Mio et 10 fichiers pour le log opérationnel, 1 Gio par fichier de
journal événementiel et 100 fichiers. Les valeurs exactes et leurs minima sont
regroupées dans [`docs/configuration.md`](docs/configuration.md).

### Local, LAN et cloud

Chaque provider déclare une zone, et cette zone doit aussi être autorisée pour
son usage. Un serveur local reste borné au loopback:

```toml
[policy]
stt_zones = ["local"]

[stt.providers.local]
type = "openai-compatible"
base_url = "http://127.0.0.1:8000/v1"
model = "large-v3"
language = "fr"
timeout_s = 120
zone = "local"
```

Pour un serveur du LAN, déclarer la zone et les réseaux privés ou explicitement
fiables. `trusted_networks` sert notamment aux plages non classées privées:

```toml
[stt]
provider = "lan"

[stt.providers.lan]
type = "openai-compatible"
base_url = "http://10.0.0.10:8000/v1"
model = "large-v3"
language = "fr"
timeout_s = 120
zone = "lan"

[policy]
stt_zones = ["lan"]
trusted_networks = ["10.0.0.0/24"]
```

Pour un service cloud, utiliser HTTPS, une zone `internet` et une source de
secret. Un fallback doit être autorisé lui aussi:

```toml
[stt]
provider = "local"
fallback = "cloud"

[stt.providers.cloud]
type = "openai-compatible"
base_url = "https://api.openai.com/v1"
model = "gpt-4o-transcribe"
language = "fr"
timeout_s = 60
zone = "internet"
api_key_env = "OPENAI_API_KEY"

[policy]
stt_zones = ["local", "internet"]
```

Chaque provider STT peut exposer un check explicite et des retries bornés:

```toml
health_enabled = true
health_endpoint = "/health"
retries = 1
retry_backoff_ms = 250
```

Le timeout du provider couvre l'opération entière, backoff et retries compris.
Le `POST` STT est rejoué sur HTTP 408, 429 et 5xx; il ne l'est pas sur 401/403,
sur erreur du corps d'une réponse 2xx, ni sur erreur de transport après lecture
d'au moins un octet multipart. Une erreur de transport sans octet lu autorise un
retry, mais ce signal local ne prouve pas que le serveur n'a rien reçu. Une
réponse rejouable peut aussi arriver après traitement du corps: les retries
peuvent donc dupliquer le travail distant et ne fournissent aucune garantie
exactly-once. La matrice complète est dans
[`docs/providers.md`](docs/providers.md).

`murmur doctor` utilise exactement cet endpoint et le même contrat HTTP que le
cœur; si `health_enabled=false`, le check est ignoré et aucun `/models` n'est
dérivé. L'endpoint est résolu avant toute requête, doit conserver la même origine
(schéma, hôte et port effectif) que `base_url`, et suit la même exigence HTTPS en
zone `internet`. Le bearer n'est donc jamais envoyé à une origine de health
distincte. Chaque check externe réellement exécuté par `doctor`, résolution de
credential comprise, possède son propre plafond de cinq secondes. Les checks
étant séquentiels, leur somme dépend de la configuration et n'est pas une
deadline globale fixe. Les providers cleanup ont un schéma
plus étroit; leur instruction se configure avec `cleanup.prompt`, pas dans
`cleanup.providers.*`.

La policy désactive les proxies d'environnement, refuse les redirections et
classe toutes les adresses DNS avant la connexion. `local` ne joint que le
loopback, `lan` accepte aussi les adresses privées ou les CIDR fiables, et
`internet` autorise les trois classes. HTTP en zone `internet` est refusé sauf
opt-in `allow_insecure=true`.

### Secrets

Un provider accepte exactement une source de credential:

```toml
api_key_env = "OPENAI_API_KEY"
# ou: api_key_file = "/home/user/.config/murmur/openai.key"
# ou: api_key_command = ["pass", "show", "services/openai"]
```

Le secret n'est jamais accepté en clair dans le TOML. Un fichier doit être
régulier, appartenir à l'utilisateur et n'accorder aucun droit au groupe ou aux
autres. Son chemin doit être absolu; chaque composant est ouvert depuis `/` avec
`openat` et `O_NOFOLLOW`, donc les symlinks finaux et intermédiaires sont refusés
et une substitution concurrente du chemin ne redirige pas la lecture. Le helper
est exécuté sans shell, avec un délai interne de 10 secondes et une annulation de
tout son groupe de processus. `doctor` lui superpose un plafond local de 5
secondes, qui devient son timeout effectif dans ce contexte. Il hérite de
l'environnement usuel nécessaire à `pass`, `secret-tool`, GnuPG, D-Bus ou SSH,
mais jamais des variables nommées par `api_key_env` dans un provider STT ou
cleanup, actif, fallback ou inactif. Les snapshots de session, événements,
diagnostics et résultats persistants n'incluent pas le secret.

## Commandes

```text
murmur start [--clipboard-only]
murmur stop
murmur cancel
murmur toggle [--clipboard-only]
murmur status [--json]
murmur doctor [--json]
murmur transcribe [--json] [--events] fichier.wav
murmur cleanup [--json] [--events]
murmur dictate [--sink stdout|file] [--json] [--events]
```

- `start` lance en arrière-plan une unique session; `--clipboard-only` force une
  livraison au presse-papiers sans injection.
- `stop` termine proprement la capture, puis exécute transcription, pipeline et
  sink.
- `cancel` annule avant livraison, arrête les enfants et interdit le sink.
- `toggle` démarre au repos et arrête pendant `starting` ou `recording`.
- `status` affiche l'état actif ou le dernier résultat; `--json` expose le
  document versionné.
- `doctor` vérifie seulement les providers, outils et interfaces exigés par la
  configuration effective; un check en échec retourne 1. Chaque check externe
  borné réellement exécuté, résolution de credential comprise, dispose de 5 s et
  les checks indépendants continuent après expiration. Comme ils sont
  séquentiels, leur budget cumulé vaut `5 s × nombre de checks bornés réellement
  exécutés`; un credential en échec saute le health correspondant. Le calcul
  conditionnel complet est dans [`docs/doctor.md`](docs/doctor.md).
- `transcribe` traite un WAV PCM s16 mono 16 kHz existant, sans capture ni
  desktop. Il exige un fichier régulier, refuse symlink final, FIFO et device,
  puis lie découpage et upload au descripteur validé plutôt qu'au chemin mutable.
- `cleanup` lit stdin et écrit stdout, sans STT; si le cleanup est désactivé,
  l'entrée est conservée.
- `dictate` capture au premier plan jusqu'à EOF ou signal et utilise `stdout` si
  `--sink` est omis. `--sink file` exige un chemin `sink.file`. Une mort
  spontanée de `pw-record` termine immédiatement en `audio_failed`, nettoie la
  capture et interdit STT et sink.

`--json` écrit un objet unique sur stdout. `--events` réserve stderr à un flux
JSONL sans transcript. Les codes de sortie sont 0 en succès, 1 en erreur
d'exécution ou diagnostic négatif, et 2 en erreur d'usage. Le contrat détaillé
est dans [`docs/cli.md`](docs/cli.md).

## Hyprland

Exemple générique, à adapter aux variables et touches de la configuration
Hyprland locale:

```ini
bind = $mod, D, exec, murmur toggle
bind = $mod SHIFT, D, exec, murmur cancel
bind = $mod CTRL, D, exec, murmur start --clipboard-only
```

`toggle` suffit pour un raccourci push-to-toggle. Pour deux raccourcis séparés,
lier `murmur start` et `murmur stop`. Murmur n'a besoin ni d'un service systemd
utilisateur ni du checkout: `start` ré-exécute le binaire installé et la session
se termine après livraison ou annulation.

## Sinks et presse-papiers

`sink.mode` accepte `paste`, `type`, `clipboard`, `stdout`, `file` et `auto`.
`auto` devient le choix explicite `sink.auto_mode="paste"|"type"`, indépendamment
du profil de données. `paste` copie puis émet uniquement le premier raccourci
configuré avec `wtype`. Si la liste de raccourcis est vide, ou avec
`clipboard_mode="none"`, il frappe directement et ne colle jamais un ancien
presse-papiers.

Le texte provider est filtré à la frontière des modes actifs `paste` et `type`,
avant leur `wl-copy` et leur `wtype`: le défaut `type_newlines="space"` ne laisse
passer ni CR/LF, ESC, DEL, C0 ou C1; les tabulations deviennent des espaces. Le
mode `dangerous-keep` est l'opt-in explicite pour injecter des LF. L'ancien
`keep` est refusé. Les sinks passifs `clipboard`, `stdout`, `file` et
`--clipboard-only` transportent toujours le texte sans ce filtre.

`sink.clipboard_mode` contrôle les copies annexes:

- `raw-and-clean`: après processing réussi, copie le STT brut puis le texte
  final dans la phase de livraison;
- `clean-only`: copie seulement le texte final;
- `none`: ne lance jamais `wl-copy`;
- `transient`: copie pour la livraison puis tente `wl-copy --clear`.

Annulation, résultat vide et erreur de processing ne produisent aucune copie. La
fenêtre d'annulation est fermée atomiquement avant la première copie; un échec de
copie rend `failed/sink_failed`. Les modes persistants ne sont pas effacés sur
erreur et Murmur ne prétend jamais purger un gestionnaire d'historique ayant déjà
capturé le texte. Un mode effectif `paste` avec raccourci et `transient` est
refusé avant capture: `wtype` ne confirme que l'émission du raccourci, pas la
consommation du collage avant le clear. Les alternatives sont `clean-only` pour
un collage persistant, `type+transient` pour un clear best-effort, ou
`type+none` pour ne jamais exposer le texte au presse-papiers. Une liste de
raccourcis vide et `paste+none` passent directement par la frappe.
`start --clipboard-only` exige `raw-and-clean` ou `clean-only`.
Pour une exécution sans intégration desktop, configurer `sink.mode="stdout"`,
`sink.clipboard_mode="none"` et `ui.mode="none"`, ou utiliser les commandes
composables.

Chaque copie, frappe, clear ou tentative de collage est bornée à 5 secondes. Le
budget est une constante de sûreté, pas une option de
configuration. Un `wl-copy` ou `wtype` bloqué voit son groupe de processus tué et
récolté; la session rend `failed/sink_failed`, libère verrou et socket, puis une
nouvelle session peut démarrer.

## UI

`ui.mode="notify"` utilise les notifications freedesktop, remplace la
notification précédente et se dégrade en no-op si D-Bus est absent. Le titre
est « Murmur », précédé d'un rond vert, orange en cas d'annulation et rouge en
cas d'erreur, avec un texte court : « Écoute en cours », « Transcription en
cours », « Dictée terminée » ou « Dictée annulée ».
`ui.mode="osd"` lance `murmur-osd` et lui transmet des états JSONL sans texte
dicté; sa panne n'interrompt ni transcription ni livraison. L'installation OSD
est volontairement optionnelle car elle ajoute Python/GTK/gtk-layer-shell.
`ui.mode="none"` désactive l'affichage. `ui.sound=true` joue un bip freedesktop
au début effectif et à l'arrêt de la capture, en best-effort.

À la fin d'une session, l'UI draine toujours l'état terminal avant fermeture. Le
helper OSD reçoit ensuite EOF mais reste visible pendant le délai terminal
configuré avant de sortir; Murmur impose une grâce bornée puis tue et récolte un
helper bloqué. Une fermeture sans terminal reste immédiate, avec au plus 250 ms
de grâce OSD.

Le groupe `[ui.osd]` configure `helper`, `position`, `success_timeout_ms` et
`error_timeout_ms`. `general.ascii_quotes=true` sélectionne les guillemets ASCII
dans le pipeline texte.

Le protocole et les options du helper sont dans
[`docs/osd-protocol.md`](docs/osd-protocol.md).

## Chunking live

Les sessions utilisent désormais des segments de **5 secondes**, transcrits
pendant la capture. Une recherche de pause sur les dernières **500 ms** rapproche
les coupes des silences. Le premier envoi peut partir après environ **6 secondes**
de PCM : seule une seconde de réserve reste en attente pour éviter une petite
queue finale. L'indicateur micro reste en « écoute » pendant le travail STT.

```toml
[stt]
chunk_seconds = 5
live_chunking = true
chunk_pause_ms = 500
chunk_overlap_ms = 0
chunk_workers = 2
```

Les valeurs explicites d'une configuration existante restent prioritaires :
remplacer notamment un ancien `chunk_seconds = 60` pour adopter les fenêtres
courtes. `live_chunking=false` sélectionne le batch, et `chunk_seconds=0`
désactive le découpage et le live implicite. `transcribe` et `dictate` utilisent
le découpage post-capture, avec les mêmes règles de pause et de concurrence.

`stt.chunk_workers` borne la
concurrence et les résultats sont réordonnés avant une unique passe globale de
cleanup/plugin et une unique livraison. Les files de segments sont bornées, mais
leur contre-pression ne retarde pas l'arrêt: le stop interrompt `pw-record` sur un
plan de contrôle distinct, puis la finalisation attend le drainage des workers.

`chunk_pause_ms=0` permet une comparaison à coupe fixe. Le recouvrement
`chunk_overlap_ms=500` est expérimental et opt-in : la déduplication de mots aux
frontières reste heuristique. Les mesures JSONL `timing` séparent STT, attente en
file, cleanup et délai après stop, sans texte dicté. Le protocole de comparaison,
les outils de replay et les résultats mesurés sont dans
[`docs/latency.md`](docs/latency.md).

`stt.max_text_bytes=16777216` borne par défaut à 16 Mio le cumul UTF-8 de toutes
les réponses réussies et de leurs séparateurs, principal et fallback compris.
`stt.max_segments=4096` borne les requêtes, maps, slices et fichiers retenus pour
le tri ou une reprise. Un dépassement annule les appels STT restants, retourne
`server_error` et interdit toute livraison, même si la récupération partielle est
activée. La concurrence effective tient aussi compte des réponses provider de 4
Mio afin de ne pas matérialiser 64 corps maximaux en parallèle.

Par défaut, un segment en échec fait échouer la session et un fallback reprend
tous les segments, sans mélange implicite. `allow_partial=true` et
`allow_mixed_providers=true` sont des récupérations dégradées explicites et
tracées. Voir [`docs/audio.md`](docs/audio.md) et
[`docs/providers.md`](docs/providers.md).

## Plugin Markdown

Le plugin Markdown local est déterministe et désactivé par défaut:

```toml
[plugin]
enabled = true
mode = "markdown"
timeout_s = 2

[policy]
plugin_zones = ["local"]
```

Il ne transforme que les commandes introduites explicitement par `commande stt`
et terminées par `fin du titre`, `fin du gras`, `fin de l'italique` ou `fin de
liste`. La ponctuation ordinaire reste du contenu; les numéros explicites des
items doivent suivre `1, 2, 3...`. Une commande invalide conserve le texte
pré-plugin et ajoute un warning. Les plugins non locaux reçoivent une capability
HTTP netguard au lieu d'obtenir le réseau par simple déclaration de zone. La
grammaire et l'interface exactes sont dans
[`docs/markdown.md`](docs/markdown.md).

## Diagnostics et fichiers

Avant le premier usage desktop:

```sh
murmur doctor
murmur doctor --json
murmur status --json
```

Le runtime privé est `$XDG_RUNTIME_DIR/murmur` en `0700`; sans
`XDG_RUNTIME_DIR`, Murmur utilise uniquement `/run/user/<euid>/murmur` si
`/run/user/<euid>` existe déjà, est un répertoire réel appartenant à l'EUID et
est en `0700`. Sinon la commande échoue en demandant une configuration explicite;
aucun repli sous `/tmp` n'est utilisé. Toutes les commandes résolvent ce même
chemin déterministe. Il contient le verrou, le socket IPC, `last-result.json` et son marqueur de fraîcheur
`last-result.session.json`, tous deux en `0600`. Le marqueur est invalidé
atomiquement avant chaque session puis validé après la fermeture du journal. Si
le résultat ou ce commit échoue, `status` masque l'ancien résultat et signale
`last_result_stale=true`; aucun JSON partiel n'est publié. L'échec reste
opérationnel et ne transforme pas à lui seul un résultat métier `completed` ou
`cancelled`. Le dernier résultat contient état, provider (`mixed` si plusieurs
ont contribué), classe d'erreur et warnings, jamais le transcript. Les événements
IPC sont également sans transcript.

Toute session détachée écrit d'abord un log opérationnel JSONL rotatif
`murmur.log` en `0600`, avant de rediriger le stderr brut vers `/dev/null`; les
helpers ne peuvent donc pas écrire directement dans ce fichier.
`[logging] level="info"` filtre réellement `debug`, `info`, `warn` et `error`;
`max_bytes` et `max_files` valent par défaut 1 Mio et 3. Les champs et composants
sont fermés, et les messages excluent transcript, prompt, corps HTTP, secret,
argv et causes externes brutes. Les pannes OSD ou D-Bus restent ainsi
consultables après la session.

Indépendamment, `session_journal=true` écrit `session.jsonl` et ses rotations. Il
conserve uniquement le schéma fermé des événements, jamais le transcript, un
prompt, un corps HTTP ou un secret. Ses limites par défaut sont aussi 1 Mio et 3
fichiers, réglables avec `session_max_bytes` et `session_max_files`. La file est
bornée, donc ce journal n'est pas un audit exhaustif. Une erreur d'encodage ou de fichier est mémorisée,
signalée sans donnée d'événement ni chemin, puis exposée dans les warnings du
dernier résultat. Une rotation ratée conserve le fichier courant et sera retentée.

Les WAV sont supprimés par défaut. `audio.keep_recordings=true` ou
`policy.retain_diagnostics=true` conserve au plus 10 captures et 256 Mio dans
`$XDG_STATE_HOME/murmur/recordings` (repli
`~/.local/state/murmur/recordings`), avec répertoire `0700` et fichiers `0600`.
La base doit être absolue; tous les symlinks du chemin sont refusés. La base et
les répertoires gérés doivent appartenir à l'EUID, sans écriture groupe/autres;
un store existant non conforme est refusé sans `chmod`. Création, copie et
rotation sont ancrées sur des descripteurs validés et sérialisées entre processus
par un verrou sur l'inode du store. La source est ouverte une fois sans suivre le
dernier symlink, validée par `fstat` et copiée avec une borne stricte. Son chemin
n'est supprimé que s'il désigne encore le device/inode ouvert; un remplacement
concurrent reste intact et fait échouer la rétention.
Une annulation ne conserve jamais la capture. Voir [`docs/doctor.md`](docs/doctor.md),
[`docs/ipc.md`](docs/ipc.md) et [`docs/desktop-validation.md`](docs/desktop-validation.md).

## Désinstallation

Répéter les mêmes variables de chemins qu'à l'installation. Par défaut, le
helper OSD n'est pas supprimé afin de préserver un éventuel fichier étranger du
même nom:

```sh
make uninstall PREFIX=/usr/local
```

Si le helper a été installé avec `INSTALL_OSD=1`, répéter explicitement cette
variable pour le retirer:

```sh
make uninstall PREFIX=/usr/local INSTALL_OSD=1
```

Pour un paquet, répéter également son `DESTDIR` et toute redéfinition de
`BINDIR`, `DATADIR` ou `DOCDIR`.

La désinstallation retire uniquement les fichiers livrés par Murmur et les
répertoires devenus vides. Elle ne supprime ni configuration, ni secrets, ni
état utilisateur. Pour les retirer explicitement:

```sh
rm -rf "${XDG_CONFIG_HOME:-$HOME/.config}/murmur"
rm -rf "${XDG_STATE_HOME:-$HOME/.local/state}/murmur"
```

## Licence

Murmur est distribué sous licence MIT (`SPDX-License-Identifier: MIT`). Le texte
complet se trouve dans [`LICENSE`](LICENSE) et est installé dans
`$(DOCDIR)/LICENSE`.
