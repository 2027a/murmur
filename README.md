# Murmur

Murmur est un outil de dictée push-to-talk pour Wayland. Un processus de session
capture un WAV canonique avec PipeWire, l'envoie à un provider STT compatible
OpenAI, applique un pipeline texte déterministe, puis livre le résultat par
collage, frappe, presse-papiers, stdout ou fichier. Le réseau autorisé, les
secrets, la rétention et l'interface sont explicites dans la configuration.

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
desktop; `dictate` exige seulement `pw-record` et son sink `stdout` ou `file`.

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
refusées.

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

`murmur doctor` utilise exactement cet endpoint et le même contrat HTTP que le
cœur; si `health_enabled=false`, le check est ignoré et aucun `/models` n'est
dérivé. Les providers cleanup ont un schéma plus étroit; leur instruction se
configure avec `cleanup.prompt`, pas dans `cleanup.providers.*`.

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
régulier, appartenir à l'utilisateur, ne pas être un symlink et n'accorder aucun
droit au groupe ou aux autres. Le helper est exécuté sans shell, avec un délai de
10 secondes et une annulation de tout son groupe de processus. Les snapshots de
session, événements, diagnostics et résultats persistants n'incluent pas le
secret.

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
murmur dictate --sink stdout|file [--json] [--events]
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
  configuration effective; un check en échec retourne 1.
- `transcribe` traite un WAV PCM s16 mono 16 kHz existant, sans capture ni
  desktop.
- `cleanup` lit stdin et écrit stdout, sans STT; si le cleanup est désactivé,
  l'entrée est conservée.
- `dictate` capture au premier plan jusqu'à EOF ou signal et limite le sink à
  `stdout` ou `file`.

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
du profil de données. `paste`
copie puis essaie les raccourcis configurés avec `wtype`; si leur émission
échoue, il retombe sur la frappe. Avec `clipboard_mode="none"`, il frappe
directement et ne colle jamais un ancien presse-papiers.

`sink.clipboard_mode` contrôle les copies annexes:

- `raw-and-clean`: copie le STT brut, puis le texte final;
- `clean-only`: copie seulement le texte final;
- `none`: ne lance jamais `wl-copy`;
- `transient`: copie pour la livraison puis tente `wl-copy --clear`.

Le mode transient ne purge pas un gestionnaire d'historique ayant déjà capturé
le texte. `start --clipboard-only` exige `raw-and-clean` ou `clean-only`. Pour une
exécution sans intégration desktop, configurer `sink.mode="stdout"`,
`sink.clipboard_mode="none"` et `ui.mode="none"`, ou utiliser les commandes
composables.

## UI

`ui.mode="notify"` utilise les notifications freedesktop, remplace la
notification précédente et se dégrade en no-op si D-Bus est absent. Leur titre
suit le format compact `🟢 ACTION`, avec un rond orange en cas d'annulation et
un rond rouge en cas d'erreur ou d'échec.
`ui.mode="osd"` lance `murmur-osd` et lui transmet des états JSONL sans texte
dicté; sa panne n'interrompt ni transcription ni livraison. L'installation OSD
est volontairement optionnelle car elle ajoute Python/GTK/gtk-layer-shell.
`ui.mode="none"` désactive l'affichage. `ui.sound=true` joue un bip freedesktop
au début effectif et à l'arrêt de la capture, en best-effort.

Le groupe `[ui.osd]` configure `helper`, `position`, `success_timeout_ms` et
`error_timeout_ms`. `general.ascii_quotes=true` sélectionne les guillemets ASCII
dans le pipeline texte.

Le protocole et les options du helper sont dans
[`docs/osd-protocol.md`](docs/osd-protocol.md).

## Chunking live

`stt.chunk_seconds > 0` découpe la capture après son arrêt. Ajouter
`stt.live_chunking=true` démarre la transcription de segments complets pendant
qu'un unique `pw-record` continue d'écrire. `stt.chunk_workers` borne la
concurrence et les résultats sont réordonnés avant une unique passe globale de
cleanup/plugin et une unique livraison.

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

Le runtime privé est `$XDG_RUNTIME_DIR/murmur` en `0700`; il contient le verrou,
le socket IPC et `last-result.json` en `0600`. Le dernier résultat contient état,
provider, classe d'erreur et warnings, jamais le transcript. Les événements IPC
sont également sans transcript.

Par défaut, `[logging] level="info"` et `session_journal=true` écrivent un journal
JSONL rotatif dans ce runtime. Il conserve uniquement le schéma fermé des
événements, jamais le transcript, un prompt, un corps HTTP ou un secret. Les
limites par défaut sont 1 Mio et 3 fichiers; elles se règlent avec
`session_max_bytes` et `session_max_files`. La file est bornée, donc ce journal
n'est pas un audit exhaustif.

Les WAV sont supprimés par défaut. `audio.keep_recordings=true` ou
`policy.retain_diagnostics=true` conserve au plus 10 captures et 256 Mio dans
`$XDG_STATE_HOME/murmur/recordings` (repli
`~/.local/state/murmur/recordings`), avec répertoire `0700` et fichiers `0600`.
Une annulation ne conserve jamais la capture. Voir [`docs/doctor.md`](docs/doctor.md),
[`docs/ipc.md`](docs/ipc.md) et [`docs/desktop-validation.md`](docs/desktop-validation.md).

## Désinstallation

Utiliser les mêmes chemins qu'à l'installation. Le helper OSD connu est retiré
même si `INSTALL_OSD` n'est pas répété:

```sh
make uninstall PREFIX=/usr/local
```

La désinstallation retire uniquement les fichiers livrés par Murmur et les
répertoires devenus vides. Elle ne supprime ni configuration, ni secrets, ni
état utilisateur. Pour les retirer explicitement:

```sh
rm -rf "${XDG_CONFIG_HOME:-$HOME/.config}/murmur"
rm -rf "${XDG_STATE_HOME:-$HOME/.local/state}/murmur"
```
