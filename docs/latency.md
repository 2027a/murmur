# Latence et segmentation courte

Implémentation et mesures liées au [ticket 50](https://git.2027a.net/2027a/murmur/issues/50).

## Réglage livré

```toml
[stt]
chunk_seconds = 5
live_chunking = true
chunk_pause_ms = 500
chunk_overlap_ms = 0
chunk_workers = 2
```

Ces défauts concernent les configurations qui n'ont pas déjà fixé ces valeurs.
Une configuration existante à 60 s doit être modifiée explicitement.

- **Émission précoce** : réserve d'une seconde de PCM, au lieu d'un segment
  complet. Première fenêtre disponible à environ 6 s, plus polling et attente
  éventuelle du provider. Un stop plus tôt envoie immédiatement l'audio restant.
- **Fin courte** : la coupe qui laisserait moins d'une seconde n'est pas faite.
  Le dernier segment peut donc dépasser la cible de moins d'une seconde. Aucun
  segment déjà transcrit n'est renvoyé pour cette fusion.
- **Pause** : recherche sur les dernières 500 ms avant la cible, avec 200 ms de
  faible énergie. Repli sur la coupe fixe. Aucun silence n'est supprimé.
- **Contre-pression** : un snapshot par poll ou par itération de finalisation,
  files bornées, limite de segments vérifiée avant copie. L'arrêt de PipeWire
  utilise toujours son plan de contrôle indépendant du drainage STT.
- **Batch** : `chunk_workers` s'applique aussi à la session après capture, comme
  au live et au cœur des commandes composables. Les résultats restent ordonnés.
- **UI** : les tentatives STT pendant `recording` et `stopping` restent dans les
  événements mais ne remplacent plus l'indicateur micro par « transcription »
  ou par un avertissement provisoire. Le résultat final expose les dégradations.
- **Texte** : une passe globale cleanup/plugin et une livraison finale unique.

`chunk_overlap_ms=500` permet d'évaluer un petit recouvrement. Il ajoute du PCM
précédent au début du segment suivant, sans modifier le pas nominal. La
déduplication compare au plus huit mots à une frontière réellement recouvrante,
exige au moins deux correspondances et conserve les réponses originales.
Elle ne peut pas distinguer toutes les répétitions légitimes. Le réglage reste
donc **expérimental, désactivé par défaut**. Les détails sont dans [audio.md](audio.md).

## Mesures sans transcript

Les événements JSONL `timing` sont disponibles dans `session.jsonl`, via IPC et
sur stderr avec `--events`. Ils n'ajoutent ni texte dicté, ni URL, ni prompt.

```json
{"v":1,"type":"timing","timestamp":"2026-09-29T01:00:00Z","session_id":"exemple","state":"recording","provider":"lan","partial":1,"timing":{"stage":"stt","duration_ms":810,"queue_ms":12,"audio_ms":5000}}
```

| Mesure | Début et fin |
|---|---|
| `capture_ready` | appel au recorder → PCM disponible; exclut la construction des providers |
| premier STT | premier `partial_started`; entrée dans le provider, pas preuve du premier octet sur le réseau |
| `stt.duration_ms` | tentative provider complète → validation de sa réponse; inclut HTTP, retries et backoff |
| `stt.queue_ms` | snapshot live créé ou batch prêt → worker disponible; une reprise remet son attente à zéro |
| `stt.audio_ms` | durée du WAV transmis, recouvrement compris |
| `stt_complete` | session : début de capture → résultat assemblé; commandes fichier : début du découpage → résultat assemblé |
| dernier résultat STT | dernier `partial_done`/`partial_failed` de la tentative concernée |
| `cleanup` | appel HTTP cleanup et validation; absent si désactivé ou entrée vide |
| `processing` | pipeline global, incluant cleanup et plugin |
| `delivery` | livraison, copie brute éventuelle comprise pour une session |
| `after_stop` | demande de stop acceptée → succès du sink; absent sans stop ou sans succès |

Les durées d'opérations utilisent l'horloge monotone. Les horodatages UTC servent
à corréler les événements. `queue_ms` ne mesure pas le PCM qui attend encore sa
coupe dans le WAV. Le temps HTTP ne sépare pas calcul, chargement du modèle et
attente interne du serveur : il faut les métriques du serveur pour le faire.
Ne pas assimiler toute cette durée à de l'inférence. Les bus/journaux sont bornés
et peuvent perdre des événements en cas de lecteur lent.

```sh
jq -c 'select(.type == "timing")' "$XDG_RUNTIME_DIR/murmur/session.jsonl"
murmur transcribe --events --json corpus.wav
```

Pour `dictate --json` avec sink stdout, `after_stop` finit quand le texte est
disponible pour l'objet résultat; l'encodage et l'écriture de cet objet suivent.
Les délais du bureau, du focus et de consommation du collage ne sont pas inclus
dans le simple retour du sink.

## Replay reproductible

`helpers/latency-bench` utilise les providers, la langue, le cleanup, les
commandes vocales et le plugin de la configuration Murmur effective. Il change
uniquement la segmentation, le nombre de workers et les sorties desktop : aucun
collage, aucune capture micro, aucune rétention diagnostique. Les transcripts ne
sont écrits que si `-text-out` est explicitement donné, dans des fichiers `0600`
qui doivent être absents. `-report-out` crée un JSONL de métadonnées uniquement.

```sh
go build -o /tmp/murmur-latency-bench ./helpers/latency-bench

# Même WAV, même configuration, même cleanup, cinq répétitions par mode.
for mode in batch live file; do
  /tmp/murmur-latency-bench -wav corpus.wav -mode "$mode" \
    -chunk 5 -pause-ms 500 -workers 2 -repeat 5 \
    -report-out "mesures-$mode.jsonl" -text-out "texte-$mode"
done
python3 helpers/latency-summary.py mesures-*.jsonl
```

- `batch` et `live` utilisent l'orchestrateur de session avec `FakeRecorder` et
  un sink stdout en mémoire. Le replay live avance par pas de 20 ms et demande
  stop après la durée du WAV. Le batch attend la même durée avant stop.
- `file` appelle le cœur de `transcribe`/`dictate` avec le WAV déjà disponible,
  puis le même pipeline texte. Sa durée totale est un temps fichier → texte,
  **pas** un temps après stop. Elle exclut le parsing CLI et l'écriture JSON.
- `dictate` reste un chemin capture puis batch, même si `live_chunking=true` pour
  les sessions. Pour mesurer sa capture PipeWire réelle, utiliser ses événements
  et injecter le même corpus dans une source PipeWire de test.
- Le faux recorder n'émule ni le délai matériel micro ni la charge de PipeWire.
  Sous forte contre-pression, sa croissance peut ralentir avant de compléter le
  WAV au stop. Les tests de contrôle utilisent aussi un `pw-record` simulé dont
  la croissance est indépendante pour vérifier que SIGINT n'attend pas le STT.

Comparer ensuite `-pause-ms 0`, `-overlap-ms 500` et `-workers 1`, puis des
fenêtres longues ou `-chunk 0` en batch/file. Exécuter les variantes
successivement pour ne pas se faire concurrence sur le serveur. Pour étudier
froid/chaud, contrôler le cycle de vie du moteur et noter séparément le premier
appel après déchargement confirmé. Un nouveau processus client ne rend pas le
modèle serveur froid.

Le résumé garde séparés durée du WAV, mode, modèle, langue, cleanup et réglages.
Ne pas mélanger dans une même invocation des corpus distincts de même durée ou
des configurations cleanup différentes : le booléen `cleanup` n'identifie pas
son modèle. Le p95 utilise le rang supérieur (`ceil(0.95*n)`). Les échecs sont
comptés, et exclus des percentiles de succès. `n` est le nombre de sessions pour
les phases globales, mais le nombre de tentatives pour STT et file d'attente.

### Corpus synthétique fourni

```sh
espeak-ng -v fr-fr -s 165 -f examples/latency-corpus.fr.txt -w source.wav
ffmpeg -i source.wav -ar 16000 -ac 1 -c:a pcm_s16le corpus.wav
ffmpeg -stream_loop 2 -i corpus.wav -c:a pcm_s16le corpus-long.wav
```

Le texte contient des noms propres, nombres, négations, répétitions,
code-switching et commandes vocales. Ce corpus synthétique est utile pour les
comparaisons de pipeline, pas pour conclure sur la qualité d'une voix humaine.
Compléter avec des dictées naturelles courtes/longues, du bruit, des pauses et
des commandes placées délibérément à cheval sur les frontières. Évaluer le STT
brut et le texte après cleanup séparément; compter les corrections nécessaires,
en particulier négations, montants et commandes. Aucun WER humain n'est déduit
du corpus eSpeak.

## Campagne locale du 29 septembre 2026 (UTC)

Client : Intel i7-8565U, 8 threads, Linux `7.2.7-arch1-1` x86_64,
Go `go1.27.1-X:nodwarf5`, eSpeak NG `1.52.0`. WAV ci-dessus : 21,453 s;
version répétée trois fois : 64,361 s. STT LAN configuré sous l'alias API
`whisper-1`, langue `fr`; même cleanup OpenRouter actif dans tous les essais.
Le modèle réel derrière l'alias, les versions du serveur, sa RAM/VRAM et sa charge
CPU/GPU n'ont pas été relevés. Serveur déjà sollicité, aucun cycle froid contrôlé.
Il s'agit d'observations de cette configuration, pas d'un benchmark de moteur.

### Sessions, corpus de 21,453 s

Temps en millisecondes, **médiane / p95**, cinq essais par ligne.

| Mode | Requêtes STT/session | Premier appel depuis lancement | Après stop | Cleanup seul |
|---|---:|---:|---:|---:|
| Batch, cible 60 s, fixe | 1 | 21474 / 21479 | 4132 / 5510 | 2152 / 3356 |
| Batch, cible 5 s, pause 500 ms | 5 | 21475 / 21478 | 4636 / 6161 | 1651 / 3071 |
| Live, cible 5 s, pause 500 ms | 5 | 5985 / 5990 | 3439 / 7485 | 2823 / 6772 |

Valeurs après stop par essai, dans l'ordre :

```text
batch 60 : 4132 5510 4246 3642 3565
batch  5 : 6161 4630 4636 4546 5770
live   5 : 7485 2227 3439 3362 3522
```

Le live avance effectivement le travail STT. Il améliore ici la médiane après
stop, mais **pas le p95 global**, dominé par un appel cleanup à 6,772 s. Les
segments courts en batch coûtent davantage de requêtes que le WAV entier.
Avec cinq essais, le p95 est simplement le maximum observé; ce n'est pas une
estimation robuste de la queue de distribution.

### Cœur composable, WAV déjà disponible

Même corpus, cinq essais par variante. Le temps STT global inclut découpage,
attente et assemblage, et exclut cleanup.

| Cible | Pause | Overlap | Workers | STT global médiane / p95 | Fichier → texte médiane / p95 |
|---|---:|---:|---:|---:|---:|
| 5 s | 0 ms | 0 ms | 2 | 2984 / 3065 | 4781 / 6104 |
| 5 s | 500 ms | 0 ms | 2 | 3035 / 3102 | 5016 / 5797 |
| 5 s | 500 ms | 500 ms | 2 | 3275 / 3278 | 6354 / 6759 |
| 5 s | 500 ms | 0 ms | 1 | 3478 / 3521 | 5155 / 6730 |

Deux workers offrent ici un gain STT global modeste par rapport à un, tandis que
chaque requête individuelle prend plus longtemps (médiane 1096 ms contre
698 ms). Multiplier les workers n'est donc pas une hypothèse de gain linéaire.
La file batch, contrairement au live de ces essais, est non nulle : médiane
1086 ms avec deux workers et 1563 ms avec un worker. Le plafond observé est bien
deux ou un appels actifs selon le réglage.

### Corpus de 64,361 s en live

Trois essais, 13 requêtes chacun, pas de file mesurable à la résolution de la
milliseconde. Premier appel médian à 5993 ms. Après stop : `6746 7009 6485` ms,
soit médiane 6746 et p95 7009 ms; cleanup : `6021 6238 5734` ms. Le STT restant
après stop est de l'ordre de 0,75 s; le cleanup global domine le délai final.

### Qualité et ressources

L'inspection des sorties synthétiques montre des erreurs de noms et de nombres
même sans découpage. Les fenêtres courtes ajoutent des erreurs de frontière :
sur cette voix eSpeak, la fin « septembre, fin du titre » peut être déformée en
« dans ma feuille d'étoile ». Un recouvrement de 500 ms récupère parfois cette
fin, mais produit aussi des doublons ou des formulations différentes que le
matcher conservateur ne peut pas fusionner. **Aucun gain de fidélité n'est
établi**; l'overlap reste désactivé. Une commande mal transcrite ne devient pas
correcte parce que le PCM a été conservé.

Les mesures `VmHWM` du client de replay donnent environ 18–19 Mio pour le fichier
court et jusqu'à 23,3 Mio pour le live de 64 s. La CPU client de ce dernier vaut
0,77–0,82 s par essai (user + système). Cela n'inclut pas le serveur STT/cleanup.
Les deux premières séries ont été prises avec `getrusage.ru_maxrss`, dont la
valeur héritée du harness était incohérente; elles sont exclues des chiffres RAM.
L'outil lit désormais `/proc/self/status:VmHWM` pour l'espace mémoire courant.

Le PCM de 64,361 s représente environ 2,06 Mo. Sans overlap, capture + fichiers
de segments représentent environ 4,12 Mo de données PCM cumulées sur disque,
plus les headers, hors snapshots scellés en mémoire. Il s'agit d'un calcul de
volume logique, pas d'une mesure de blocs alloués ni de débit disque. Le store
temporaire est supprimé après chaque essai. Pour une campagne de production,
relever aussi blocs disque, métriques du serveur et readiness micro réelle.

## Vérifications automatisées

`make lint test build` inclut tests Go sous `-race` et tests du helper OSD.
Les cas pertinents couvrent :

- émission dès 6 s de PCM, stops avant/à/après frontière, petite queue;
- concaténation PCM exacte sans overlap et reconstruction exacte avec overlap;
- même segmentation pause/overlap en batch et live;
- un snapshot à la fois même avec retard accumulé, limite avant copie;
- pool batch/live borné, résultats inversés réordonnés, fallback et budgets fatals;
- stop sous contre-pression, annulation et fermeture des ressources;
- cleanup et livraison uniques, timings valides et indicateur micro stable;
- assemblage français aux frontières et conservation des réponses originales.
