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

La prise en charge de `Stop` et l'observation de la sortie s'arbitrent sous le
même verrou. Si la sortie est observée alors qu'aucune action n'est engagée, son
`audio_failed` reste terminal: un `Stop` ultérieur ne signale pas le processus,
ne valide pas le WAV et ne retourne aucun fichier. Si `Stop` prend le verrou en
premier, la sortie qui suit est sa finalisation normale; son code peut alors être
non nul si le WAV final est valide. L'ordonnancement du verrou, pas un `select`
ultérieur, fixe donc le résultat d'une course.

Après readiness, la commande foreground `dictate` attend simultanément EOF ou
signal et `Done`. Une fermeture spontanée de `Done` propage immédiatement `Err`
en `audio_failed`, abandonne et supprime la capture, puis sort sans appeler STT,
pipeline ou sink. EOF et signal restent des demandes de `Stop` normal suivies du
traitement habituel.

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

Le parseur `internal/audio/wav` copie d'abord exactement la plage RIFF annoncée
dans un `memfd` privé, puis applique les seals Linux interdisant écriture,
croissance et troncature avant de la valider. Upload et découpage utilisent des
duplicatas de ce snapshot immuable, bornés à la fin RIFF ou au chunk `data`, sans
rouvrir le chemin. Une réécriture in-place de l'inode source après `Open` est donc
sans effet, et un suffixe physique après RIFF n'est jamais transmis.

L'entrée doit être un fichier régulier. Le dernier composant symbolique, les
FIFO, sockets et devices sont refusés; `O_NONBLOCK` empêche notamment une FIFO de
bloquer la validation. Un symlink dans un composant parent reste accepté, mais
il n'est résolu qu'à l'ouverture initiale: renommer ou remplacer ensuite le
chemin ne change pas la source retenue. Les descripteurs sont fermés après le
dernier upload, y compris lors d'une erreur provider.

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
`$HOME/.local/state/murmur/recordings`. `XDG_STATE_HOME` et un répertoire de
rétention explicite doivent être absolus.

Le chemin entier est parcouru depuis un descripteur de `/`, avec `O_NOFOLLOW` à
chaque niveau: tout lien symbolique, final ou intermédiaire, est refusé. La base
XDG et ses descendants gérés doivent appartenir à l'EUID et ne pas être
modifiables par le groupe ou les autres. Pour un chemin explicite, cette règle
commence à son parent immédiat. Les ancêtres antérieurs peuvent appartenir à
`root` ou être partagés, mais doivent être de vrais répertoires sans symlink.
Les composants absents sont créés un à un, sous le fd parent validé, en `0700`;
un composant existant invalide est refusé sans `chmod`. Le store final exige
exactement `0700`.

Les WAV sont créés exclusivement sous le fd du store, avec `O_EXCL` et le mode
`0600`. Inventaire, ouverture, suppression et synchronisation restent ancrés sur
ce fd. Une transaction complète prend un `flock` exclusif sur l'inode du store:
des processus concurrents ne peuvent donc ni dépasser les bornes ni supprimer
mutuellement leur capture courante. La source est ouverte une fois avec
`O_NOFOLLOW|O_NONBLOCK`, validée par `fstat` puis copiée avec une limite stricte.
L'identité du store et son chemin sans symlink sont revérifiés avant la
suppression de la capture source et avant le retour. La source n'est supprimée
que si son chemin désigne encore le device/inode ouvert avant copie; sinon la
copie diagnostique est annulée, le remplacement reste intact et l'appel échoue.

L'orchestrateur doit appeler `Release` après la dernière utilisation par le STT.
Les captures invalides ou trop courtes suivent immédiatement la même politique;
une annulation, elle, ne conserve rien.

## Découpage

`audio.Chunk` retourne des segments ordonnés et indexés. Chaque segment copie
uniquement la plage PCM validée depuis le descripteur retenu. Une durée nulle
désactive le découpage. Si le fichier tient déjà dans la limite, le résultat
référence le fichier original sans lecture, copie ni nouveau fichier. Sinon,
les coupures suivent les frontières d'échantillons et une fin plus courte que
`MinTail` est fusionnée dans le segment précédent. `audio.RemoveSegments`
ferme puis supprime uniquement les segments temporaires, jamais l'original.

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
présent. Cette vue éphémère reste attachée au fd du recorder et n'est jamais
transmise directement au STT; chaque segment est une copie immuable d'une plage
`[début, fin)` alignée
sur `BlockAlign`; le curseur suivant reprend exactement à `fin`.

Le segment complet le plus récent reste en attente. Dès qu'un autre segment
complet existe, le précédent peut alimenter le STT pendant que le même
`pw-record` continue d'écrire. Au stop, `SIGINT` finalise l'unique recorder.
`FinalizeGrowing` inspecte et écrit le header sur le même fd
`O_RDWR|O_NOFOLLOW`, puis crée le snapshot final scellé depuis cet inode sans
rouvrir le chemin. Le reliquat est extrait depuis le dernier curseur. Une queue
de moins d'une seconde est concaténée au segment retenu avant émission.

Le contrôle de capture ne partage pas la goroutine qui pousse les segments. Les
files entre le segmenter, l'orchestrateur et le pool STT restent bornées et
appliquent leur contre-pression, sans abandon ni réordonnancement. Une demande de
stop appelle toutefois immédiatement `Recording.Stop`: `SIGINT` et, si
nécessaire, l'escalade bornée vers `SIGKILL` ne dépendent donc jamais d'une place
libre dans ces files. Après la fin de `pw-record`, la goroutine de données peut
rester bloquée le temps que les workers acceptent les segments déjà produits et
le reliquat final; cette attente prolonge la finalisation de session, pas la prise
de son. Le premier appel `Stop` possède cette opération; un waiter concurrent
dont le contexte expire retourne sa propre erreur sans annuler le stop en cours.
Comme l'implémentation `pw-record`, le fake traite aussi stop et annulation tant
que sa barrière de readiness injectée reste bloquée.

Les tests font croître réellement le fichier par appends successifs, avec un
header de tailles nulles jusqu'au stop. La concaténation des chunks `data` émis
est comparée octet pour octet au chunk `data` du WAV final: aucun trou, aucun
échantillon dupliqué. Un test bloque tous les workers STT, remplit les files,
demande le stop et vérifie que `SIGINT` précède leur libération, puis contrôle les
index et le PCM après drainage. Les tests vérifient aussi qu'un seul appel
`pw-record` a lieu.
Un unique `Release` idempotent supprime segments et capture, ou conserve le WAV
final si la rétention a été explicitement activée. `Abort` tue l'unique processus
et supprime toujours l'ensemble.

La file de snapshots, de capacité 2, ne borne pas le nombre cumulé de fichiers
nécessaires au tri ou à une reprise complète sur fallback. Le STT refuse donc au
plus tard `stt.max_segments` segments, annule ses requêtes en vol et fait aborter
la capture live par l'orchestrateur. En batch, le nombre exact de segments est
calculé en O(1) depuis le WAV validé et refusé avant d'allouer le plan de tailles
ou de créer le premier fichier temporaire. Avec le défaut 4096, les fenêtres
fixes bornent déjà la durée segmentée, à un éventuel reliquat final fusionné
près; aucun second plafond de durée n'est ajouté. Le chemin sans découpage reste
un segment unique, quelle que soit la durée de son WAV, et sa sortie est bornée
par `stt.max_text_bytes`.
