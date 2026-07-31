# CLI

## Commandes

```text
murmur start [--clipboard-only]
murmur stop
murmur cancel
murmur toggle [--clipboard-only]
murmur status [--json]
murmur doctor [--json]
murmur transcribe [--json] [--events] <fichier.wav>
murmur cleanup [--json] [--events]
murmur dictate --sink stdout|file [--json] [--events]
```

Une commande inconnue, un argument surnuméraire ou un flag non reconnu affiche
l'usage sur stderr et retourne 2. Une erreur d'exécution retourne 1. `help`,
`-h` et `--help` retournent 0.

`start` résout et valide une seule configuration effective, y compris la
contrainte `--clipboard-only`, avant toute capture. Il acquiert ensuite
`session.lock`, ré-exécute le binaire avec la commande privée `__session`, puis
transfère par `ExtraFiles` le verrou, un pipe de handshake et un snapshot JSON de
la configuration. Ce snapshot contient les références `api_key_*`, jamais les
secrets résolus, et sa taille maximale est 1 MiB. L'enfant ne relit ni TOML ni
environnement de surcharge.

Le handshake réussit dès que `session.sock` répond en `starting`, avant les
helpers de secrets et la construction des providers. `start` peut donc retourner
alors que cette construction continue. `status`, `stop`, `toggle` et `cancel`
restent fonctionnels dans cette fenêtre; une erreur ultérieure produit un
événement terminal et `last-result.json`. Un timeout ou un handshake invalide
avant l'exposition du socket tue l'enfant et fait échouer `start`.

`stop` finalise l'audio puis exécute STT, pipeline et sink. `cancel` garantit
qu'aucun sink ne commence tant que la session n'a pas atteint `delivering`.
`toggle` envoie un stop en `starting` ou `recording`, démarre une nouvelle
session lorsqu'aucun socket n'existe, et est refusé dans les autres états
actifs. `toggle --clipboard-only` applique la garantie clipboard-only lors du
démarrage; si une session est déjà en écoute, le flag ne change pas son mode et
la commande l'arrête normalement.

## Status et doctor

La forme humaine de `status` est l'état seul pour une session active. Au repos,
elle affiche `idle` et le dernier résultat s'il existe. La forme JSON est stable:

```json
{"v":1,"active":false,"state":"idle","last_result":{"v":1,"session_id":"...","result":"completed","started_at":"...","finished_at":"..."}}
```

`doctor` résout la même configuration effective que `start`, puis transmet cette
valeur à `doctor.Run`. `--json` sélectionne le rapport structuré; un rapport avec
au moins un check en échec retourne 1.

## Transcribe

`transcribe` ouvre un fichier existant sans lancer de capture. Le format accepté
est le format canonique de Murmur: WAV RIFF, PCM signé 16 bits little-endian,
mono, 16 kHz. Tout autre conteneur, encodage, nombre de canaux ou fréquence
retourne 1 avec la classe `audio_failed`. Le fichier source n'est jamais modifié;
les éventuels chunks sont temporaires et supprimés.

La commande construit seulement le cœur STT et texte. Elle ne construit ni UI,
clipboard, sink d'injection, compositor, socket IPC ou recorder. Elle applique le
provider principal, le fallback, la policy réseau, les secrets, le chunking puis
une unique passe globale du pipeline cleanup/rendu/plugin. Sans `--json`, stdout
contient le transcript final suivi d'un saut de ligne.

Les options peuvent précéder ou suivre le fichier:

```sh
murmur transcribe --json audio.wav
murmur transcribe audio.wav --json
```

## Cleanup

`cleanup` lit stdin jusqu'à EOF et n'effectue aucune capture ni initialisation
STT. Si `cleanup.enabled=true`, elle utilise exactement le provider cleanup, ses
secrets et sa policy. Une panne récupérable du provider conserve l'entrée et
produit un avertissement, conformément au contrat du cleanup existant.

L'entrée est limitée à 4 MiB. Un dépassement retourne une erreur classifiée
`unavailable` sur le composant `stdin`. Pour toute entrée acceptée, les formes
JSON de `cleanup` et `transcribe` conservent toujours `transcript_raw` et
`transcript_final` conformément au schéma, y compris lorsqu'une erreur survient
après leur production; ces champs ne sont pas assimilés aux logs ou événements.

Si le cleanup est désactivé, y compris lorsque le TOML par défaut est absent, la
commande réussit et recopie les octets de stdin sans modification. La sortie JSON
contient l'avertissement `cleanup désactivé: entrée conservée sans modification`.
Si `cleanup.enabled=true` mais que `cleanup.provider` est absent ou référence une
entrée inexistante, la commande échoue avec `config_invalid`; il n'y a pas de
repli silencieux vers un autre provider.

## Dictate foreground

`dictate` est la façade de capture foreground sans session IPC. Elle force
`ui.mode=none`, `ui.sound=false` et `sink.clipboard_mode=none`; seuls `stdout` et
`file` sont acceptés. Le sink fichier utilise le chemin résolu `sink.file` et son
absence est une erreur `config_invalid`.

La capture commence avec `pw-record`, puis s'arrête proprement à EOF sur stdin ou
à la réception de SIGINT, SIGTERM ou SIGHUP. Le signal signifie ici « terminer la
capture et traiter », pas « annuler ». STT, chunking et pipeline sont ensuite les
mêmes que pour `transcribe`. Avec `--json --sink stdout`, l'objet JSON constitue
la représentation du sink stdout et le transcript texte n'est pas écrit une
seconde fois. Avec `--sink file --json`, le fichier est livré et l'objet résultat
reste seul sur stdout.

## Résultat JSON v1

`--json` écrit exactement un objet sur stdout, en succès comme en échec. Sa forme
normative est [`cli-result.schema.json`](cli-result.schema.json). Tous les champs
sont toujours présents; les tableaux vides restent `[]` et l'absence d'erreur est
`null`.

```json
{
  "v": 1,
  "command": "transcribe",
  "success": true,
  "provider": "local",
  "model": "large-v3",
  "language": "fr",
  "transcript_raw": "bonjour brut",
  "transcript_final": "Bonjour.",
  "segment_count": 1,
  "segments": [
    {
      "index": 0,
      "provider": "local",
      "model": "large-v3",
      "language": "fr",
      "transcript": "bonjour brut"
    }
  ],
  "warnings": [],
  "error": null
}
```

Pour `transcribe` et `dictate`, `transcript_raw` est le transcript STT assemblé
et `transcript_final` la sortie du pipeline global. Pour `cleanup`, ils désignent
respectivement stdin et la sortie cleanup ou son repli. En cas de providers
mixtes autorisés, le provider global peut être vide; chaque segment conserve son
provider, modèle et langue effectifs.

Chaque avertissement expose `class`, `provider`, `component` et `message`.
`error` expose seulement `class`, `component` et un message contrôlé, sans cause
interne ni secret. Un échec d'exécution retourne 1 après l'objet JSON. Une erreur
d'usage ne produit pas d'objet et retourne 2.

## Événements JSONL

`--events` écrit les événements de progression exclusivement sur stderr, un objet
JSON v1 par ligne. Ils couvrent démarrage, capture éventuelle, segments STT,
provider choisi, dégradations, livraison éventuelle et terminaison. Comme le bus
d'événements de session, ils ne contiennent jamais de transcript.

Lorsque `--events` est actif, aucun message d'erreur humain n'est ajouté sur
stderr: l'erreur terminale est un événement JSONL et, avec `--json`, son détail
est dans l'objet unique sur stdout. Sans `--events`, stderr reste réservé aux
erreurs humaines. Il n'y a donc jamais de mélange entre flux résultat et flux
d'événements.

## Stdout détaché

Le sink `stdout` ne peut pas être reconstruit après le détachement: il doit
écrire dans le flux exact ouvert par l'appelant. Pour ce seul sink effectif, `start`
transmet donc son descripteur stdout à l'enfant `setsid`; stdin et stderr sont
redirigés vers `/dev/null`. Les autres sinks n'héritent pas stdout.

Conséquence volontaire: avec un pipe ou une substitution de commande, le lecteur
observe la fin de fichier seulement lorsque la session terminale ferme son
descripteur. `murmur start` retourne dès que le socket est prêt, mais le flux
stdout reste logiquement attaché à la livraison. Cela préserve le texte sans
processus relais, fichier temporaire ni extension du protocole IPC.

Le choix d'héritage de stdout et la construction du sink utilisent le même
snapshot déjà concrétisé. Une modification du fichier ou de l'environnement
entre le lancement du parent et celui de `__session` ne peut donc pas faire
diverger stdout, sink et policy.
