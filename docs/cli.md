# CLI

## Commandes

```text
murmur start [--clipboard-only]
murmur stop
murmur cancel
murmur toggle [--clipboard-only]
murmur status [--json]
murmur doctor [--json]
murmur ui
murmur transcribe [--json] [--events] <fichier.wav>
murmur cleanup [--json] [--events]
murmur dictate [--sink stdout|file] [--json] [--events]
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

## Flux de barre QuickShell

`murmur ui` est un processus persistant lancé par le module QML, avec stdin
ouvert. Il transmet des instantanés JSONL sur stdout et attend leur numéro
`seq` suivi d'un retour à la ligne sur stdin. Il ne démarre aucune capture.
Le processus appartient au shell, pas à une dictée; EOF sur stdin le termine.
Les détails et limites sont dans [quickshell.md](quickshell.md).

## Status et doctor

La forme humaine de `status` est l'état seul pour une session active. Au repos,
elle affiche `idle` et le dernier résultat s'il existe. Si sa liste de warnings
contient `component="session-journal"`, la forme humaine ajoute `journal dégradé`;
la forme JSON expose le warning contrôlé complet. Si une session plus récente a
commencé mais que son résultat ou le commit final n'a pas pu être persisté,
`status` n'expose jamais l'ancien résultat: la forme humaine indique
`dernier résultat périmé: persistance incomplète`, et le JSON expose
`last_result_stale=true` sans `last_result`. La forme JSON est stable:

```json
{"v":1,"active":false,"state":"idle","last_result":{"v":1,"session_id":"...","result":"completed","started_at":"...","finished_at":"..."}}
```

Une erreur de persistance reste une erreur opérationnelle de la session, mais ne
reclasse pas son résultat métier terminal: un `completed` ou `cancelled` reste
tel quel dans la valeur retournée par l'orchestrateur. Tant que son commit n'est
pas confirmé, `status` préfère toutefois le diagnostic de péremption à un JSON
potentiellement ancien. Si l'invalidation initiale échoue, `start` retourne 1
avant l'exposition du socket et aucun résultat antérieur n'est périmé.

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

Dans la sortie JSON, une transcription multi-provider expose
`provider="mixed"`; `model` et `language` globaux sont alors vides, tandis que
chaque entrée de `segments` conserve son provider, son modèle et sa langue.

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
`file` sont acceptés. Si `--sink` est omis, `stdout` est utilisé indépendamment
de `sink.mode` dans la configuration. `--sink file` utilise le chemin résolu
`sink.file`; son absence est toujours une erreur `config_invalid`.

La capture commence avec `pw-record`, puis s'arrête proprement à EOF sur stdin ou
à la réception de SIGINT, SIGTERM ou SIGHUP. Le signal signifie ici « terminer la
capture et traiter », pas « annuler ». STT, chunking et pipeline sont ensuite les
mêmes que pour `transcribe`. Avec `--json` et le sink stdout, implicite ou
explicite, l'objet JSON constitue la représentation du sink et le transcript
texte n'est pas écrit une seconde fois. Avec `--sink file --json`, le fichier est
livré et l'objet résultat reste seul sur stdout.

Si `pw-record` s'arrête de lui-même après readiness, `dictate` n'attend pas EOF:
la commande retourne 1 avec `audio_failed`, supprime la capture et n'appelle ni
provider ni sink. Pour interrompre sa goroutine de lecture, le binaire ferme son
stdin système, dont il possède le descripteur. Un lecteur injecté n'est jamais
fermé par supposition: il est annulé seulement s'il implémente
`CancelRead() error`. Sans cette abstraction, une lecture non annulable peut
rester bloquée jusqu'au retour propre du lecteur, mais elle ne retarde pas la fin
de la commande.

## Résultat JSON v1

`--json` écrit exactement un objet sur stdout, en succès comme en échec. Sa forme
normative Draft 2020-12 est
[`cli-result.schema.json`](cli-result.schema.json). Tous les champs sont toujours
présents; les tableaux vides restent `[]` et l'absence d'erreur est `null`.
`success=true` impose `error=null`; `success=false` impose une erreur structurée.

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
respectivement stdin et la sortie cleanup ou son repli; `segment_count` vaut
alors zéro et `segments` reste vide. Pour une transcription, `segment_count` est
la taille de `segments`, dont les index originaux sont uniques et strictement
croissants, avec des trous possibles en récupération partielle. En cas de
providers mixtes autorisés, le provider global vaut `mixed`, avec `model` et
`language` globaux vides; chaque segment conserve son provider, modèle et langue
effectifs. Avec un seul provider, ces métadonnées globales correspondent à
celles des segments. Les égalités de compte, l'ordre des index et ces relations
de provenance complètent le schéma, qui ne peut pas comparer ces champs entre
eux.

Avec `chunk_overlap_ms>0`, un segment peut aussi porter `overlap_words` (2 à 8).
Son `transcript` reste la réponse originale. Pour reconstruire `transcript_raw`,
retirer ce nombre de mots initiaux délimités par des espaces Unicode, conserver
le reste, puis joindre les segments par une espace. Sans ce champ, aucun mot
n'est retiré. Le champ n'est jamais présent sur le premier segment ni après un
trou d'index. Cette extension optionnelle du JSON v1 est décrite dans le schéma.

Chaque avertissement expose `class`, `provider`, `component` et `message`.
Les dégradations cleanup/plugin conservent leur classe (`server_error`,
`timeout`, `auth_failed`, `policy_blocked`, `unavailable`, etc.) dans ce JSON et
dans l'événement `degraded` correspondant. Leur message et leur composant sont
contrôlés par le pipeline, jamais dérivés du texte brut de l'erreur.
`error` expose seulement `class`, `component` et un message contrôlé, sans cause
interne ni secret. Un échec d'exécution retourne 1 après l'objet JSON. Une erreur
d'usage ne produit pas d'objet et retourne 2.

## Événements JSONL

`--events` écrit les événements de progression exclusivement sur stderr, un objet
JSON v1 par ligne. Ils couvrent démarrage, capture éventuelle, segments STT,
provider choisi, dégradations, livraison éventuelle et terminaison. Comme le bus
d'événements de session, ils ne contiennent jamais de transcript.

Les événements `timing` ajoutent les durées STT, file d'attente, cleanup et
processing. `dictate` ajoute readiness, livraison et délai après stop. Les
horodatages `partial_started` identifient la première tentative, et les
`partial_done`/`partial_failed` son achèvement. Voir [latency.md](latency.md).

Lorsque `--events` est actif, aucun message d'erreur humain n'est ajouté sur
stderr: l'erreur terminale est un événement JSONL et, avec `--json`, son détail
est dans l'objet unique sur stdout. Sans `--events`, stderr reste réservé aux
erreurs humaines. Il n'y a donc jamais de mélange entre flux résultat et flux
d'événements.

Une erreur d'écriture de stderr arrête immédiatement l'émission et force le code
de sortie 1. Une erreur métier existante reste prioritaire: sa classe, son
composant et son message contrôlé sont conservés dans le résultat JSON. Si le
métier avait réussi, le résultat JSON devient un échec `internal` du composant
`io`, sans reprendre le détail du writer. Sans `--json`, ni stdout ni stderr
ne constituent un canal sûr pour ce diagnostic: seul le code de sortie le
signale et le transcript n'est pas écrit.

Un writer peut avoir écrit une partie du dernier objet avant de signaler son
échec. Ce fragment JSON est irréparable sans protocole transactionnel;
l'émetteur le laisse tel quel, mais n'ajoute ensuite ni événement ni texte
humain. Les objets terminés qui le précèdent restent du JSONL valide.

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
