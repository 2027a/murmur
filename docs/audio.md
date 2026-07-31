# Capture audio

## Contrats

`internal/audio` expose les contrats utilisés par l'orchestrateur:

```go
type Recorder interface {
    Start(context.Context, RecordOptions) (Recording, error)
}

type Recording interface {
    Ready() <-chan error
    Done() <-chan struct{}
    Err() error
    Stop(context.Context) (WavFile, error)
    Abort() error
}
```

`Ready` est fermé sans valeur lorsque des octets d'échantillons sont réellement
présents dans le chunk `data`. En cas d'échec, il fournit une erreur puis est
fermé. La seule création du fichier ou de son en-tête RIFF ne vaut pas readiness.

`Done` est fermé dès que le processus de capture est terminé. Après sa fermeture,
`Err` retourne l'échec terminal mémorisé; une sortie spontanée, même avec un code
zéro, est classée `audio_failed`. Un unique waiter possède `Process.Wait`: `Stop`,
`Abort` et les observateurs attendent tous le même signal sans second wait.

`Stop` et `Abort` envoient `SIGINT`, attendent pendant un delai borne, puis
escaladent vers `SIGKILL` avec une seconde attente bornee. `Stop` ne valide le
WAV que si la sortie gracieuse a reussi. `Abort` supprime toujours la capture,
même si la rétention diagnostique est active. L'annulation du contexte passé à
`Start` a le même effet qu'un abandon et ne laisse pas `pw-record` actif.

## Format et commande

Le format canonique est PCM signé 16 bits, mono, 16 kHz. L'adaptateur lance:

```text
pw-record --media-category Capture --media-role Communication \
  --rate 16000 --channels 1 --format s16 [--target SOURCE] recording.wav
```

Les processus externes passent exclusivement par `sysexec.Executor`. Son
implémentation réelle place chaque commande asynchrone dans un groupe de
processus afin que signaux et annulations atteignent aussi ses enfants.

Le parseur `internal/audio/wav` vérifie les tailles RIFF et les paramètres PCM,
accepte les chunks inconnus et leur octet de padding, et rejette les fichiers
tronqués, ambigus ou non alignés sur un échantillon.

## Capture vide

`RecordOptions.MinimumDuration` fixe le seuil de capture utile. Sa valeur par
défaut est `100ms`. Un chunk `data` vide ou une durée sous ce seuil produit
`no_speech`; un fichier mal formé produit `audio_failed`.

## Fichiers temporaires et diagnostics

Une capture réussie reste disponible jusqu'à `WavFile.Release`. Cet appel:

- supprime la capture par défaut;
- la copie dans le répertoire diagnostique si `RetentionOptions.Keep` est vrai;
- borne ce répertoire par nombre de fichiers et taille totale, en supprimant les
  fichiers les plus anciens.

Sans limites explicites, la rétention conserve au plus 10 fichiers et 256 MiB.
Le répertoire par défaut est
`$XDG_STATE_HOME/murmur/recordings`, ou
`$HOME/.local/state/murmur/recordings`. Répertoire et fichiers sont forcés aux
modes `0700` et `0600`.

L'orchestrateur doit appeler `Release` après la dernière utilisation par le STT.
Les captures invalides ou trop courtes suivent immédiatement la même politique;
une annulation, elle, ne conserve rien.

## Découpage

`audio.Chunk` retourne des segments ordonnés et indexés. Une durée nulle
désactive le découpage. Si le fichier tient déjà dans la limite, le résultat
référence le fichier original sans lecture, copie ni nouveau fichier. Sinon,
les coupures suivent les frontières d'échantillons et une fin plus courte que
`MinTail` est fusionnée dans le segment précédent. `audio.RemoveSegments`
supprime uniquement les segments temporaires, jamais l'original.

`audio.FakeRecorder` rejoue progressivement le PCM d'une fixture dans un WAV en
croissance et implémente le même cycle de vie; les tests amont n'ont donc besoin
ni de PipeWire ni de `pw-record`. `DoneGate` et `DoneErr` simulent une terminaison
spontanée après readiness, pour les captures normale et live.

### Snapshots live

`audio.LiveRecorder` est un contrat optionnel, utilisé uniquement lorsque
`stt.live_chunking` vaut `true`. Un unique processus `pw-record` écrit le même
WAV pendant toute la capture. `wav.Snapshot` localise le chunk `data` sans faire
confiance aux tailles RIFF/data provisoires, borne la lecture à la taille physique
observée et ignore le dernier octet tant qu'un échantillon PCM complet n'est pas
présent. Chaque segment est une copie immuable d'une plage `[début, fin)` alignée
sur `BlockAlign`; le curseur suivant reprend exactement à `fin`.

Le segment complet le plus récent reste en attente. Dès qu'un autre segment
complet existe, le précédent peut alimenter le STT pendant que le même
`pw-record` continue d'écrire. Au stop, `SIGINT` finalise l'unique recorder,
`wav.Open` relit le header final, puis le reliquat est extrait depuis le dernier
curseur. Une queue de moins d'une seconde est concaténée au segment retenu avant
émission. La concaténation reconstruit uniquement l'en-tête RIFF et conserve tous
les blocs PCM dans l'ordre.

Les tests font croître réellement le fichier par appends successifs, avec un
header de tailles nulles jusqu'au stop. La concaténation des chunks `data` émis
est comparée octet pour octet au chunk `data` du WAV final: aucun trou, aucun
échantillon dupliqué. Ils vérifient aussi qu'un seul appel `pw-record` a lieu.
Un unique `Release` idempotent supprime segments et capture, ou conserve le WAV
final si la rétention a été explicitement activée. `Abort` tue l'unique processus
et supprime toujours l'ensemble.
