# Configuration

## Chargement

Ordre de précédence, du moins prioritaire au plus prioritaire:

1. valeurs compilées de `config.Defaults()`;
2. `$XDG_CONFIG_HOME/murmur/config.toml`;
3. variables d'environnement autorisées;
4. `config.Overrides`, destiné aux futurs flags CLI.

Une clé TOML inconnue est une erreur `config_invalid`. Un chemin explicite passé
à `config.Resolve` doit exister; seul le chemin XDG implicite peut être absent.

Les répertoires applicatifs XDG sont créés avec un unique `mkdir`, puis contrôlés
avec `lstat`. Le runtime n'accepte aucun symlink et son parent XDG exige `0700`.
Le répertoire de configuration peut être un lien vers des dotfiles si le lien et
sa cible appartiennent à l'utilisateur courant et si la cible n'est modifiable
ni par le groupe ni par les autres. Sans `XDG_RUNTIME_DIR`, le fallback
`murmur-<uid>` n'est accepté que sous un répertoire temporaire protégé par son
propriétaire ou par le sticky bit.

## Schéma

| Clé | Défaut | Valeurs ou usage |
|---|---|---|
| `general.profile` | `local` | `offline`/`local`, `lan`, `lan-cloud-cleanup`, `cloud`, `cloud-no-history` |
| `general.command_pack` | `auto` | `auto`, `fr`, `en` |
| `general.ascii_quotes` | `false` | rend `" texte "` au lieu des guillemets typographiques |
| `audio.source` | vide | source PipeWire par défaut si vide |
| `audio.keep_recordings` | `false` | conservation diagnostique opt-in |
| `stt.provider` | `local` | nom dans `stt.providers` |
| `stt.fallback` | vide | nom optionnel dans `stt.providers` |
| `stt.chunk_seconds` | `0` | entier positif, 0 désactive le découpage |
| `stt.live_chunking` | `false` | snapshots et STT pendant la capture; exige `chunk_seconds > 0` |
| `stt.chunk_workers` | `2` | concurrence STT live bornée, de 1 à 64 |
| `stt.allow_partial` | `false` | récupération partielle explicite avec warning |
| `stt.allow_mixed_providers` | `false` | fallback par segment explicite et tracé |
| `cleanup.enabled` | `false` | active le cleanup |
| `cleanup.provider` | vide | nom dans `cleanup.providers` |
| `cleanup.prompt` | vide | instruction cleanup; vide utilise le prompt strict intégré |
| `plugin.enabled` | `false` | active explicitement l'étape de grammaire |
| `plugin.mode` | `plain` | `plain` ou `markdown` |
| `plugin.timeout_s` | `2` | timeout strictement positif |
| `policy.stt_zones` | `["local"]` | zones autorisées pour le STT |
| `policy.cleanup_zones` | `["local"]` | zones autorisées pour le cleanup |
| `policy.plugin_zones` | `["local"]` | zones autorisées; `lan`/`internet` exigent le contrat externe à capability netguard |
| `policy.trusted_networks` | `[]` | CIDR classés `lan` |
| `policy.retain_diagnostics` | `false` | rétention diagnostique opt-in |
| `sink.mode` | `paste` | `paste`, `type`, `clipboard`, `stdout`, `file`, `auto` |
| `sink.auto_mode` | `paste` | choix explicite `paste` ou `type` lorsque `mode=auto` |
| `sink.clipboard_mode` | `clean-only` | `raw-and-clean`, `clean-only`, `none`, `transient` |
| `sink.paste_shortcuts` | cascade usuelle | raccourcis de collage ordonnés |
| `sink.type_newlines` | `keep` | `keep`, `space`, `marker` |
| `sink.file` | vide | obligatoire pour le sink `file` |
| `ui.mode` | `notify` | `osd`, `notify`, `none` |
| `ui.sound` | `false` | feedback sonore best-effort |
| `ui.osd.helper` | `murmur-osd` | commande ou chemin du helper OSD |
| `ui.osd.position` | `top-center` | `top-left`, `top-center`, `center` |
| `ui.osd.success_timeout_ms` | `2500` | délai avant masquage d'un résultat non fautif, 0 autorisé |
| `ui.osd.error_timeout_ms` | `4000` | délai avant masquage d'une erreur, 0 autorisé |
| `logging.level` | `info` | `debug`, `info`, `warn`, `error` |
| `logging.session_journal` | `true` | journal JSONL des événements de session, sans texte |
| `logging.session_max_bytes` | `1048576` | taille maximale d'un fichier, minimum 1024 |
| `logging.session_max_files` | `3` | fichier actif inclus, de 1 à 100 |

Chaque entrée de `stt.providers.<nom>` accepte:

| Clé | Contrainte |
|---|---|
| `type` | actuellement `openai-compatible` uniquement |
| `base_url` | URL HTTP(S) absolue, sans userinfo, query ni fragment |
| `model` | modèle transmis au provider |
| `language` | langue, vide pour la détection automatique |
| `prompt` | prompt de contexte STT |
| `timeout_s` | entier strictement positif |
| `zone` | `local`, `lan`, `internet` |
| `allow_insecure` | opt-in requis pour HTTP en zone `internet` |
| `health_endpoint` | URL absolue, chemin depuis l'origine (`/health`) ou relatif à `base_url` |
| `health_enabled` | active explicitement le health pour le cœur et `doctor`; défaut `false` |
| `retries` | essais supplémentaires, de 0 à 10 |
| `retry_backoff_ms` | attente entre essais, de 0 à 60000 ms |
| `api_key_env` | nom d'une variable contenant le secret |
| `api_key_file` | fichier régulier appartenant à l'utilisateur, sans symlink ni droits groupe/autres |
| `api_key_command` | tableau argv d'un helper explicite, sans shell |

Une entrée `cleanup.providers.<nom>` accepte uniquement `type`, `base_url`,
`model`, `timeout_s`, `zone`, `allow_insecure` et les trois sources
`api_key_*`. Les champs STT `language`, `prompt`, `health_*`, `retries` et
`retry_backoff_ms` y sont des clés inconnues. Le prompt cleanup appartient à
`cleanup.prompt`, pas au provider.

Une seule source `api_key_*` peut être définie. `config.SecretResolver` reçoit
un `CommandRunner`; la session et `doctor` l'adaptent à `internal/sysexec` afin
d'exécuter le helper sans shell et dans un groupe de processus contrôlé. Un
fichier de credential et la sortie d'un helper sont limités à 64 KiB; le
dépassement produit une erreur classifiée sans inclure le secret.

## Environnement

Seules les variables suivantes modifient la configuration. Les autres variables
`MURMUR_*` sont ignorées.

```text
MURMUR_PROFILE
MURMUR_COMMAND_PACK
MURMUR_ASCII_QUOTES
MURMUR_AUDIO_SOURCE
MURMUR_KEEP_RECORDINGS
MURMUR_STT_PROVIDER
MURMUR_STT_FALLBACK
MURMUR_STT_CHUNK_SECONDS
MURMUR_STT_LIVE_CHUNKING
MURMUR_STT_CHUNK_WORKERS
MURMUR_STT_ALLOW_PARTIAL
MURMUR_STT_ALLOW_MIXED_PROVIDERS
MURMUR_CLEANUP_ENABLED
MURMUR_CLEANUP_PROVIDER
MURMUR_PLUGIN_ENABLED
MURMUR_PLUGIN_MODE
MURMUR_PLUGIN_TIMEOUT_S
MURMUR_STT_ZONES
MURMUR_CLEANUP_ZONES
MURMUR_PLUGIN_ZONES
MURMUR_TRUSTED_NETWORKS
MURMUR_RETAIN_DIAGNOSTICS
MURMUR_SINK_MODE
MURMUR_SINK_AUTO_MODE
MURMUR_SINK_CLIPBOARD_MODE
MURMUR_SINK_FILE
MURMUR_UI_MODE
MURMUR_UI_SOUND
MURMUR_OSD_HELPER
MURMUR_OSD_POSITION
MURMUR_OSD_SUCCESS_TIMEOUT_MS
MURMUR_OSD_ERROR_TIMEOUT_MS
MURMUR_LOG_LEVEL
MURMUR_SESSION_JOURNAL
MURMUR_SESSION_MAX_BYTES
MURMUR_SESSION_MAX_FILES
```

Les listes sont séparées par des virgules. Les booléens utilisent les formes
acceptées par `strconv.ParseBool`.

Les variables nommées par `api_key_env` transportent les credentials mais ne
font pas partie des surcharges de configuration ci-dessus. La session retire
tous les noms `api_key_env` de l'environnement transmis à `pw-record`,
`wl-copy`, `wtype`, l'OSD et les lecteurs sonores, y compris les noms de
providers inactifs. Seul l'exécuteur séparé des helpers `api_key_command` reçoit
l'environnement complet.

## Validation croisée

La résolution refuse notamment:

- une référence vers un provider absent;
- un profil absent de la matrice ci-dessous;
- `command_pack = "auto"` sans langue STT principale explicite `fr`/`en`, ou
  avec un fallback dans une autre langue;
- une zone d'un provider actif absente de la policy correspondante, avec la
  classe `policy_blocked` plutôt que `config_invalid`;
- un plugin actif dont la zone déclarée est absente de `policy.plugin_zones`;
- un provider `local` dont l'URL n'est manifestement pas locale;
- une IP publique déclarée `lan`;
- HTTP en zone `internet` sans `allow_insecure = true`;
- le sink `file` sans chemin;
- un CIDR invalide ou une durée négative;
- `stt.live_chunking = true` sans durée de segment, ou un nombre de workers
  hors de l'intervalle 1 à 64;
- `clipboard-only` avec un presse-papiers `none` ou `transient`.

La dernière règle est appliquée séparément par
`config.ValidateClipboardOnly(cfg, true)` car `clipboard-only` est une garantie
de session, pas un réglage persistant.

## Profils de données

Les profils fournissent des défauts de policy et de rétention. Ils ne choisissent
plus le mode d'injection; `sink.auto_mode` remplit ce rôle sans ambiguïté.

| Profil | `stt_zones` | `cleanup_zones` et `plugin_zones` | Défaut d'historique local |
|---|---|---|---|
| `offline` ou `local` | `local` | `local` | diagnostics et WAV non retenus |
| `lan` | `local`, `lan` | `local`, `lan` | diagnostics et WAV non retenus |
| `lan-cloud-cleanup` | `local`, `lan` | toutes zones | diagnostics et WAV non retenus |
| `cloud` | toutes zones | toutes zones | diagnostics et WAV non retenus |
| `cloud-no-history` | toutes zones | toutes zones | idem, plus `clipboard_mode=none` |

Un défaut de profil ne remplace jamais une valeur définie dans le TOML,
l'environnement ou `config.Overrides`, y compris `false`, une liste ou
`clipboard_mode`. La précédence reste donc: profil effectif comme défaut faible,
puis valeurs explicites selon leur couche. `cloud-no-history` ne prétend pas
effacer l'historique d'un gestionnaire de presse-papiers externe ni la rétention
d'un provider distant; une surcharge explicite peut également réactiver une
rétention locale.

`config.Resolve` retourne un `sink.mode` concret lorsque `mode=auto`; le snapshot
de session ne transporte donc pas `auto`.

## Journal de session

Lorsque `logging.session_journal=true`, la session écrit
`$XDG_RUNTIME_DIR/murmur/session.jsonl`, puis `session.jsonl.1`, etc. La rotation
se fait avant dépassement de `session_max_bytes` et conserve au plus
`session_max_files` fichiers. Chaque ligne est exactement un `events.Event` v1:
aucun champ transcript, prompt, corps HTTP ou secret n'existe dans ce schéma.
Le flux est borné et peut abandonner des événements lors d'une surcharge; il ne
doit pas être traité comme un audit exhaustif. `logging.level` règle les messages
du logger applicatif; le journal structuré garde les transitions quel que soit ce
niveau.

`api_key_command` est exécuté sans shell, dans un groupe de processus dédié et
avec une limite de 10 secondes et une capture stdout bornée à 64 KiB.
L'annulation de la session interrompt le groupe entier. La sortie du helper et
les causes brutes ne sont jamais reprises dans un événement ou un résultat
persistant.

## Découpage live

Le mode live est entièrement opt-in. Avec `live_chunking = false`, le chemin
reste celui du découpage post-capture historique: une capture unique, puis
`audio.Chunk`, ou un seul fichier sans copie quand `chunk_seconds = 0`.

En mode live, `chunk_seconds` devient la taille des plages PCM extraites du WAV
écrit par l'unique `pw-record`. Au plus
`chunk_workers` appels STT sont actifs. Les résultats sont réordonnés par index
avant assemblage, indépendamment de leur ordre de terminaison. Un échec de
segment fait échouer la session et interdit toute livraison par défaut.
`allow_partial = true` omet explicitement les segments en échec et produit
`WarningPartial`. `allow_mixed_providers = true` autorise le fallback par
segment; chaque provider reste présent dans `MultiResult.Segments` et un
`WarningMixedProviders` est ajouté si plusieurs providers ont contribué.
Sans cet opt-in, le premier échec du principal relance tous les segments sur le
fallback, ce qui évite un transcript implicitement mixte.
