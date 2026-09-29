# Configuration

## Chargement

Ordre de précédence, du moins prioritaire au plus prioritaire:

1. valeurs compilées de `config.Defaults()`;
2. `$XDG_CONFIG_HOME/murmur/config.toml`;
3. variables d'environnement autorisées;
4. `config.Overrides`, destiné aux futurs flags CLI.

Une clé TOML inconnue est une erreur `config_invalid`. Un chemin explicite passé
à `config.Resolve` doit exister; seul le chemin XDG implicite peut être absent.

Lorsqu'il existe, `config.toml` est ouvert en suivant les liens symboliques
finaux ou intermédiaires, puis copié dans un snapshot mémoire borné à 1 Mio avant
décodage. La même politique s'applique au chemin XDG implicite et à tout
`ConfigPath` explicite.
La cible doit être régulière, appartenir à l'EUID et ne pas être modifiable par
le groupe ou les autres; `0600` et `0644` sont donc valides, contrairement à
`0620`, `0602` ou `0666`. Device, inode, taille, mtime et ctime sont contrôlés
avant et après la copie; le chemin est ensuite rouvert et son inode comparé. Une
réécriture in-place, un remplacement de cible ou un retargeting concurrent du
symlink fait échouer le préflight avant interprétation du TOML, helper, capture
ou réseau.

Le runtime est parcouru composant par composant avec `openat`, `O_NOFOLLOW` et
des contrôles `fstat`; sa création reste ancrée au descripteur du parent validé.
Il n'accepte aucun symlink, son parent exige l'EUID et le mode `0700`, et son
sous-répertoire `murmur` est réel, possédé par l'EUID et en `0700`. Un ancêtre
modifiable par le groupe ou les autres est refusé sauf s'il porte le sticky bit.
Le répertoire de configuration peut donc être symlinké vers un dépôt de
dotfiles. Sans `XDG_RUNTIME_DIR`, seul `/run/user/<euid>` est essayé et il doit
déjà satisfaire ce contrat; sinon Murmur demande de définir explicitement la
variable. Aucun chemin prévisible sous `/tmp` n'est utilisé. Le chemin reste
déterministe afin que toutes les commandes retrouvent la même session.

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
| `stt.chunk_seconds` | `5` | de 0 à 86400 s; 0 désactive le découpage et le live implicite |
| `stt.live_chunking` | `true` | snapshots et STT pendant la capture des sessions; exige `chunk_seconds > 0` |
| `stt.chunk_pause_ms` | `500` | recherche d'une pause avant la coupe, de 0 à 1000 ms et strictement inférieure au segment; 0 = fixe |
| `stt.chunk_overlap_ms` | `0` | PCM précédent ajouté au segment, de 0 à 1000 ms et strictement inférieur à `chunk_seconds*1000 - chunk_pause_ms`; expérimental |
| `stt.chunk_workers` | `2` | concurrence STT batch et live bornée, de 1 à 64 |
| `stt.max_text_bytes` | `16777216` | budget cumulé des textes STT en octets UTF-8, de 1 à 67108864 |
| `stt.max_segments` | `4096` | nombre maximal de segments conservés, de 1 à 65536 |
| `stt.allow_partial` | `false` | récupération partielle explicite avec warning |
| `stt.allow_mixed_providers` | `false` | fallback par segment explicite et tracé |
| `cleanup.enabled` | `false` | active le cleanup |
| `cleanup.provider` | vide | nom dans `cleanup.providers` |
| `cleanup.prompt` | vide | instruction cleanup; vide utilise le prompt strict intégré |
| `plugin.enabled` | `false` | active explicitement l'étape de grammaire |
| `plugin.mode` | `plain` | `plain` ou `markdown` |
| `plugin.timeout_s` | `2` | timeout de 1 à 86400 s |
| `policy.stt_zones` | `["local"]` | zones autorisées pour le STT |
| `policy.cleanup_zones` | `["local"]` | zones autorisées pour le cleanup |
| `policy.plugin_zones` | `["local"]` | zones autorisées; `lan`/`internet` exigent le contrat externe à capability netguard |
| `policy.trusted_networks` | `[]` | CIDR classés `lan` |
| `policy.retain_diagnostics` | `false` | rétention diagnostique opt-in |
| `sink.mode` | `paste` | `paste`, `type`, `clipboard`, `stdout`, `file`, `auto` |
| `sink.auto_mode` | `paste` | choix explicite `paste` ou `type` lorsque `mode=auto` |
| `sink.clipboard_mode` | `clean-only` | `raw-and-clean`, `clean-only`, `none`, `transient` |
| `sink.paste_shortcuts` | liste usuelle | liste ordonnée parmi `ctrl+shift+v`, `shift+insert`, `ctrl+v`; seul le premier raccourci est émis |
| `sink.type_newlines` | `space` | `space`, `marker`, `dangerous-keep`; ce dernier autorise explicitement les LF en injection active |
| `sink.file` | vide | obligatoire pour le sink `file` |
| `ui.mode` | `notify` | `osd`, `notify`, `none` |
| `ui.sound` | `false` | bips best-effort au début et à l'arrêt de la capture |
| `ui.osd.helper` | `murmur-osd` | commande ou chemin du helper OSD |
| `ui.osd.position` | `top-center` | `top-left`, `top-center`, `center` |
| `ui.osd.success_timeout_ms` | `2500` | délai avant masquage d'un résultat non fautif, de 0 à 86400000 ms |
| `ui.osd.error_timeout_ms` | `4000` | délai avant masquage d'une erreur, de 0 à 86400000 ms |
| `logging.level` | `info` | `debug`, `info`, `warn`, `error` |
| `logging.max_bytes` | `1048576` | taille maximale de `murmur.log`, de 1024 à 16777216 octets |
| `logging.max_files` | `3` | fichiers de log opérationnel, actif inclus, de 1 à 10 |
| `logging.session_journal` | `true` | journal JSONL des événements de session, sans texte |
| `logging.session_max_bytes` | `1048576` | taille maximale d'un fichier, de 1024 à 1073741824 octets |
| `logging.session_max_files` | `3` | fichier actif inclus, de 1 à 100 |

Le budget des commandes externes de livraison n'est pas configurable: chaque
copie, clear, frappe ou tentative de collage est borné à 5 secondes. Une
expiration tue et récolte le groupe de processus, puis produit
`failed/sink_failed`. Il n'existe volontairement pas de clé `sink.timeout_*`:
ce délai protège le cycle de vie de la session et ne constitue pas une policy
utilisateur.

Chaque entrée de `stt.providers.<nom>` accepte:

| Clé | Contrainte |
|---|---|
| `type` | actuellement `openai-compatible` uniquement |
| `base_url` | URL HTTP(S) absolue, sans userinfo, query ni fragment |
| `model` | modèle non vide transmis au provider |
| `language` | langue, vide pour la détection automatique |
| `prompt` | prompt de contexte STT |
| `timeout_s` | entier de 1 à 86400 s |
| `zone` | `local`, `lan`, `internet` |
| `allow_insecure` | opt-in requis pour HTTP en zone `internet` |
| `health_endpoint` | URL de même origine que `base_url`, chemin depuis l'origine (`/health`) ou relatif à `base_url` |
| `health_enabled` | active explicitement le health pour le cœur et `doctor`; défaut `false` |
| `retries` | essais supplémentaires, de 0 à 10 |
| `retry_backoff_ms` | attente entre essais, de 0 à 60000 ms |
| `api_key_env` | nom d'une variable contenant le secret |
| `api_key_file` | chemin absolu vers un fichier régulier appartenant à l'utilisateur, sans symlink final/intermédiaire ni droits groupe/autres |
| `api_key_command` | tableau argv d'un helper explicite, sans shell; premier élément non vide |

Une entrée `cleanup.providers.<nom>` accepte uniquement `type`, `base_url`,
`model`, `reasoning_effort`, `timeout_s`, `zone`, `allow_insecure` et les trois sources
`api_key_*`. Les champs STT `language`, `prompt`, `health_*`, `retries` et
`retry_backoff_ms` y sont des clés inconnues. Le prompt cleanup appartient à
`cleanup.prompt`, pas au provider. Son `model` doit être non vide et son
`timeout_s` reste borné de 1 à 86400 s.

`reasoning_effort` règle le niveau de raisonnement du modèle de cleanup :

```toml
[cleanup.providers.openrouter]
# ... base_url, model et credentials habituels ...
reasoning_effort = "low"
```

Clé absente ou vide : aucun paramètre envoyé, comportement du provider conservé.
Valeurs : `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`.
La valeur est transmise telle quelle dans `reasoning_effort` de la requête Chat
Completions (OpenAI/OpenRouter). Les niveaux acceptés dépendent du modèle :
`none` demande la désactivation, `low` un effort réduit. Murmur ne remplace pas
un niveau refusé par un autre; un refus suit le repli cleanup habituel, avec
conservation du texte et warning. Le [catalogue OpenRouter](https://openrouter.ai/api/v1/models)
expose les niveaux pris en charge dans `reasoning.supported_efforts`.

Une seule source `api_key_*` peut être définie. `config.SecretResolver` reçoit
un `CommandRunner`; la session et `doctor` l'adaptent à `internal/sysexec` afin
d'exécuter le helper sans shell et dans un groupe de processus contrôlé. Un
fichier de credential et la sortie d'un helper sont limités à 64 KiB; le
dépassement produit une erreur classifiée sans inclure le secret.

`api_key_file` est toujours ancré à `/`; les chemins relatifs sont refusés car
leur base serait ambiguë entre le processus, la configuration et les commandes
composables. Chaque composant est ouvert avec `openat` et `O_NOFOLLOW`. Le
fichier final est ouvert en mode non bloquant, validé par `fstat`, puis lu et
borné depuis ce même descripteur: un FIFO ne bloque pas et le remplacement
concurrent d'un parent ou du chemin ne peut pas rediriger la lecture.

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
MURMUR_STT_MAX_TEXT_BYTES
MURMUR_STT_MAX_SEGMENTS
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
MURMUR_LOG_MAX_BYTES
MURMUR_LOG_MAX_FILES
MURMUR_SESSION_JOURNAL
MURMUR_SESSION_MAX_BYTES
MURMUR_SESSION_MAX_FILES
```

Les listes sont séparées par des virgules. Les booléens utilisent les formes
acceptées par `strconv.ParseBool`.

Les variables nommées par `api_key_env` transportent les credentials mais ne
font pas partie des surcharges de configuration ci-dessus. La session, les
commandes composables et `doctor` retirent tous les noms `api_key_env` de
l'environnement transmis aux commandes externes et aux helpers
`api_key_command`, y compris ceux des providers fallback ou inactifs et ceux du
cleanup désactivé. La comparaison des noms suit Unix et reste sensible à la
casse; toutes les affectations dupliquées d'un nom configuré sont supprimées.
Le reste de l'environnement est conservé afin que `pass`, `secret-tool`, D-Bus,
GnuPG, SSH et les agents de secrets continuent de fonctionner.

## Validation croisée

La résolution refuse notamment:

- une référence vers un provider absent;
- un `model` vide ou blanc dans une entrée STT ou cleanup;
- un profil absent de la matrice ci-dessous;
- `command_pack = "auto"` sans langue STT principale explicite `fr`/`en`, ou
  avec un fallback dans une autre langue;
- une zone d'un provider actif absente de la policy correspondante, avec la
  classe `policy_blocked` plutôt que `config_invalid`;
- un plugin actif dont la zone déclarée est absente de `policy.plugin_zones`;
- un provider `local` dont l'URL n'est manifestement pas locale;
- une IP publique déclarée `lan`;
- HTTP en zone `internet` sans `allow_insecure = true`;
- un `health_endpoint` d'une autre origine que `base_url`, ou HTTP en zone
  `internet` sans le même opt-in `allow_insecure`;
- le sink `file` sans chemin;
- le sink `clipboard` avec `clipboard_mode=none` ou `transient`;
- un mode effectif `paste` avec au moins un raccourci et
  `clipboard_mode=transient`, car `wtype` ne confirme pas la consommation du
  collage avant le clear;
- un raccourci absent de `ctrl+shift+v`, `shift+insert`, `ctrl+v`;
- un `api_key_command` dont le nom d'exécutable est vide;
- un `sink.type_newlines` autre que `space`, `marker` ou `dangerous-keep`;
- un CIDR invalide, une durée supérieure à 24 h, ou un délai négatif;
- `stt.live_chunking = true` sans durée de segment, ou un nombre de workers
  hors de l'intervalle 1 à 64;
- un budget de texte hors de 1 octet à 64 Mio, ou un plafond de segments hors de
  1 à 65536;
- un log opérationnel hors de 1024 octets à 16 Mio par fichier ou de 1 à 10
  fichiers;
- un journal hors de 1024 octets à 1 Gio par fichier ou de 1 à 100 fichiers;
- `clipboard-only` avec un presse-papiers `none` ou `transient`.

La dernière règle est appliquée séparément par
`config.ValidateClipboardOnly(cfg, true)` car `clipboard-only` est une garantie
de session, pas un réglage persistant.

`config.Validate` est le préflight déterministe commun. `start` l'exécute dans
le parent avant création du runtime, verrou, pipes, fork ou socket; la session le
répète avant credential helper, UI et recorder. `doctor` appelle la même fonction
et s'arrête sur le même verdict avant tout check d'outil, source, credential ou
réseau. Les constructeurs conservent leurs validations défensives pour leurs
usages directs.

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

Le mode historique `sink.type_newlines="keep"` n'est pas conservé comme alias:
il est refusé au preflight. Utiliser `space` pour le comportement sûr par défaut,
`marker` pour rendre les ruptures visibles, ou `dangerous-keep` lorsqu'une
injection active de LF est réellement voulue.

## Logs et journal de session

Toute session détachée prépare `murmur.log` dans le runtime Murmur résolu ci-dessus avant le
démarrage du child, avant de rediriger le stderr brut vers `/dev/null`, et y
conserve uniquement les diagnostics opérationnels JSONL du logger. Le fichier
actif et ses rotations `murmur.log.1`, etc. sont en `0600`; `max_bytes` et
`max_files` imposent respectivement 1 Mio et 3 fichiers par défaut, avec plafonds
de 16 Mio et 10 fichiers. `logging.level` filtre réellement `debug`, `info`,
`warn` et `error`. Chaque entrée ne contient que `time`, `level`, un `component`
choisi dans une liste fermée et un `message` borné. Les appels ne doivent jamais
recevoir transcript, prompt, corps HTTP, secret, argv ou cause externe brute;
les dégradations OSD et D-Bus utilisent donc des messages d'opération constants.
Une erreur d'écriture du logger est ignorée sans tentative de se logger elle-même.

Lorsque `logging.session_journal=true`, la session écrit
`session.jsonl`, puis `session.jsonl.1`, etc. dans ce même runtime. La rotation
se fait avant dépassement de `session_max_bytes` et conserve au plus
`session_max_files` fichiers. Chaque ligne est exactement un `events.Event` v1:
aucun champ transcript, prompt, corps HTTP ou secret n'existe dans ce schéma.
Le flux est borné et peut abandonner des événements lors d'une surcharge; il ne
doit pas être traité comme un audit exhaustif. Le journal structuré garde les
transitions quel que soit `logging.level`. Il possède son propre writer et ne
dépend ni de `murmur.log` ni de sa rotation; réciproquement, le log opérationnel
reste actif lorsque le journal est désactivé ou dégradé. Une panne du journal est
signalée une seule fois au logger par un message constant et apparaît comme warning
`session-journal` dans le dernier résultat. Les causes retournées localement par
`Close` peuvent contenir le chemin runtime; elles ne sont jamais persistées ni
envoyées au logger.

`api_key_command` est exécuté sans shell, dans un groupe de processus dédié et
avec une limite de 10 secondes et une capture stdout bornée à 64 KiB.
L'annulation de la session interrompt le groupe entier. La sortie du helper et
les causes brutes ne sont jamais reprises dans un événement ou un résultat
persistant. Avant chaque helper, la politique d'environnement décrite plus haut
s'applique au snapshot complet des providers, sans exception pour le provider
en cours.

## Découpage live

Le mode live est actif par défaut pour les sessions, avec des fenêtres de 5 s.
Une valeur explicite existante reste inchangée. `chunk_seconds=0` désactive le
live si `live_chunking` n'a pas été précisé; `live_chunking=true` explicite avec
une durée nulle reste une erreur de configuration. Avec `live_chunking = false`, le chemin
reste celui du découpage post-capture historique: une capture unique, puis
`audio.Chunk`, ou un seul fichier sans copie quand `chunk_seconds = 0`.

Les clés `chunk_pause_ms` et `chunk_overlap_ms` s'appliquent aussi au batch et aux
commandes composables. Leurs overrides d'environnement sont
`MURMUR_STT_CHUNK_PAUSE_MS` et `MURMUR_STT_CHUNK_OVERLAP_MS`. Une durée nulle de
segment ignore ces réglages. Le détecteur de pause utilise 200 ms d'énergie RMS
faible, dans la fenêtre précédant la coupe, sans suppression d'audio. Le
recouvrement est ajouté avant le curseur, sans réduire le pas nominal; il peut
donc augmenter la durée transmise au provider. L'assemblage ne déduplique que des
segments adjacents marqués comme recouvrants. Voir [audio.md](audio.md).

En mode live, `chunk_seconds` devient le pas cible des plages PCM extraites du WAV
écrit par l'unique `pw-record`. Une seconde de PCM supplémentaire suffit à émettre
un segment; aucun segment complet n'est retenu ni retranscrit pour fusionner une
petite fin. Au plus
`chunk_workers` appels STT sont demandés. La limite effective est le minimum de
ce réglage et de `ceil(max_text_bytes / 4194304)`: puisque chaque réponse peut
atteindre 4 Mio, les corps en vol restent ainsi du même ordre de grandeur que le
budget global. Les résultats sont réordonnés par index avant assemblage,
indépendamment de leur ordre de terminaison. Un échec de segment fait échouer la
session et interdit toute livraison par défaut.
`allow_partial = true` omet explicitement les segments en échec et produit
`WarningPartial`. `allow_mixed_providers = true` autorise le fallback par
segment; chaque provider reste présent dans `MultiResult.Segments` et un
`WarningMixedProviders` est ajouté si plusieurs providers ont contribué. La
provenance globale vaut alors `mixed`, quel que soit leur ordre.
Sans cet opt-in, le premier échec du principal relance tous les segments sur le
fallback, ce qui évite un transcript implicitement mixte. Le batch utilise aussi
le pool lorsque `chunk_workers>1`; avec un worker, il conserve son arrêt anticipé
sur erreur. Le live doit toujours recevoir le reste de la capture pour un replay
complet éventuel.

`max_text_bytes` compte `len(text)` pour chaque réponse réussie, donc les octets
de son encodage UTF-8 en Go, ainsi que les espaces ajoutés entre réponses. Le
même compteur couvre batch, live, principal et fallback; les réponses primaires
ensuite écartées par un fallback restent comptées. Toute addition vérifie le
reste disponible avant de modifier le compteur. Un dépassement retourne
`server_error` avec le message stable `volume cumulé des transcriptions
supérieur à la limite configurée`, annule immédiatement le pool et ne produit
jamais de résultat partiel, même avec `allow_partial=true`.

Les canaux bornés ne limitent que la file instantanée. `max_segments` borne en
plus les slices de requêtes, les maps de résultats et les fichiers nécessaires à
un éventuel fallback. Son dépassement suit la même politique sans livraison,
avec le message `nombre de segments STT supérieur à la limite configurée` (ou
`nombre de segments audio supérieur à la limite configurée` si le producteur
live atteint sa borne avant le pool). Une
limite de durée séparée n'est pas nécessaire: en mode segmenté, le nombre de
fenêtres fixes borne déjà la durée, à l'éventuel reliquat final fusionné près;
sans découpage, une session ne produit qu'une réponse soumise au budget de texte.
