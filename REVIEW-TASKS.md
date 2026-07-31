# Tâches issues de la revue approfondie

Revue effectuée le 31 juillet 2026 sur `main` à partir du commit `e41f4f7`.
Ce fichier ne décrit que des changements actionnables. Les tâches sont ordonnées
par priorité, puis par impact. Elles sont conçues pour pouvoir être confiées
séparément à un agent.

**Avancement au 31 juillet 2026:** les 35 tâches sont terminées et documentées
ci-dessous. La validation finale passe: `go mod verify`, `make lint`, `go test
-count=1 ./...`, `go test -race ./...`, `go test -cover ./...`,
`make installcheck`, tests Python du helper OSD, validation yamllint du workflow,
builds linux/amd64 et linux/arm64, et `git diff --check`. Deux contrôles externes
n'ont pas été exécutés: le test de blocage Forgejo par push de branches
volontairement cassées, car aucun commit/push n'a été demandé, et un scanner
SPDX, car aucun n'est installé; l'identifiant MIT exact est toutefois suivi et
testé par recherche.

Vérifications passées pendant la revue:

```sh
go mod verify
go vet ./...
go test ./...
go test -race ./...
go test -cover ./...
make lint
make installcheck
GOOS=linux GOARCH=amd64 go build -o /tmp/opencode/murmur-amd64 ./cmd/murmur
GOOS=linux GOARCH=arm64 go build -o /tmp/opencode/murmur-arm64 ./cmd/murmur
```

## P0 - Sécurité ou blocage majeur

### MUR-001 - Lier l'upload STT aux octets WAV effectivement validés

**Statut (31 juillet 2026): terminé.** `wav.Open` copie la plage RIFF dans un
`memfd` privé scellé contre écriture, croissance et troncature, puis valide et
expose uniquement ce snapshot. Les symlinks finaux, FIFO et devices sont refusés.
L'upload et le découpage ignorent remplacement, réécriture in-place et suffixe
hors RIFF. `FinalizeGrowing` écrit et relit le même fd stable avant de produire
son snapshot final. Tests ajoutés pour substitution, `WriteAt`, seals, suffixe
canari, chunking, politique de type et nettoyage.
Vérifications passées: `go test -race ./internal/audio/wav ./internal/core
./internal/stt ./internal/acceptance`, `go test -race ./internal/audio
./internal/audio/pwrecord ./internal/session`, `go test ./...` et
`git diff --check`.

**Constat:** `wav.Open` valide un descripteur puis le ferme. Le client STT rouvre
ensuite `wav.File.Path()` et copie le fichier entier. Une substitution entre les
deux ouvertures peut donc faire envoyer un autre fichier. Même sans course, les
octets situés après la fin RIFF validée sont transmis au provider. Les chemins de
découpage rouvrent eux aussi la source après validation.

**Références:** `internal/audio/wav/wav.go:32-38`,
`internal/audio/wav/wav.go:122-140`, `internal/audio/wav/wav.go:310-352`,
`internal/core/transcribe.go:18-49`, `internal/stt/openai.go:114-132`,
`internal/stt/openai.go:240-258`.

**Travail attendu:** faire porter à `wav.File` une source stable ou créer, dès la
validation, un snapshot canonique privé. L'upload et le découpage doivent lire
exactement les octets validés, avec une borne explicite, sans rouvrir un chemin
mutable. Définir et appliquer une politique pour les symlinks, FIFO et devices.

**Critères d'acceptation:** remplacer atomiquement le chemin après validation ne
change pas le corps multipart; un marqueur ajouté après la fin RIFF n'est jamais
envoyé; les segments contiennent uniquement le PCM validé; les descripteurs et
snapshots temporaires sont fermés et supprimés sur tous les chemins d'erreur.

**Vérification:** ajouter des tests avec substitution de fichier et suffixe
canari, avec et sans chunking, puis exécuter `go test -race ./internal/audio/wav
./internal/core ./internal/stt ./internal/acceptance`.

### MUR-002 - Empêcher la fuite de bearer par `health_endpoint`

**Statut (31 juillet 2026): terminé.** Le endpoint de santé est résolu et
validé avant credential ou réseau, doit rester sur la même origine (schéma,
hôte et port effectif) et suit la règle HTTPS de `base_url`; `allow_insecure`
n'autorise pas le cross-origin. Les contrôles sont partagés par la validation,
le coeur, `doctor` et défensivement par le client STT. Les tests couvrent deux
serveurs TLS observant les headers, les ports par défaut et les endpoints
relatifs ou invalides. Vérifications passées: `go test -race ./internal/config
./internal/stt ./internal/doctor`, `go test -race ./internal/core
./internal/httpurl`, `go test ./...`, `go vet` sur les packages touchés et
`git diff --check`.

**Constat:** la validation impose HTTPS à `base_url` en zone `internet`, mais une
URL absolue HTTP reste valide dans `health_endpoint`. Une URL absolue vers une
autre origine reçoit également le bearer du provider. Cela contredit la garantie
documentée selon laquelle un credential n'est pas envoyé vers une destination
différente.

**Références:** `internal/config/validate.go:166-185`,
`internal/config/validate.go:192-220`, `internal/stt/openai.go:181-237`,
`docs/providers.md:10-30`, `README.md:174-183`.

**Travail attendu:** résoudre et valider l'URL de santé avant toute requête,
appliquer la règle HTTPS de `base_url`, et interdire par défaut l'envoi du bearer
à une autre origine. Si un health check cross-origin est réellement nécessaire,
prévoir un opt-in et un credential distincts plutôt que de réutiliser le secret
STT.

**Critères d'acceptation:** un health HTTP en zone `internet` est refusé quand
`allow_insecure=false`; aucune origine différente ne reçoit le bearer par
défaut; les contrôles existent dans la validation et défensivement dans le
constructeur STT; `doctor` et le coeur appliquent la même règle.

**Vérification:** tests avec deux serveurs HTTP/TLS enregistrant les headers, puis
`go test -race ./internal/config ./internal/stt ./internal/doctor`.

### MUR-003 - Rendre l'arrêt live indépendant du backpressure STT

**Statut (31 juillet 2026): terminé.** La demande de stop emprunte désormais un
plan de contrôle indépendant de la boucle de segmentation et déclenche
immédiatement l'arrêt de `pw-record`; la boucle de données reste seule
responsable de l'ordre et les files STT restent bornées. Un test synchronisé
sature les files avec deux workers bloqués, vérifie `SIGINT` avant drainage,
l'unicité des index et le PCM exact. Vérifications passées: `go test -race
./internal/audio/pwrecord ./internal/stt ./internal/session`, `go test -race
./internal/audio`, stress ciblé 20 fois sous race, `go vet` des packages
concernés, `go test ./...` et `git diff --check`.

**Constat:** `liveRecording.run` appelle `emit` dans sa propre boucle de contrôle.
Lorsque `parts` est pleine et que le pipeline STT ne consomme plus, `emit` bloque
et la boucle ne peut plus recevoir `r.stop`. Le microphone peut continuer à
enregistrer jusqu'à la fin d'un appel provider, potentiellement pendant son
timeout complet.

**Références:** `internal/audio/pwrecord/live.go:75-81`,
`internal/audio/pwrecord/live.go:119-147`, `internal/audio/pwrecord/live.go:166-173`,
`internal/stt/stt.go:264-304`, `internal/session/orchestrator.go:291-375`.

**Travail attendu:** séparer le plan de contrôle du flux de segments. Une demande
de stop doit interrompre `pw-record` dans un délai borné, même lorsque toutes les
files et tous les workers STT sont bloqués. La solution doit conserver une borne
mémoire et l'ordre des segments.

**Critères d'acceptation:** SIGINT est envoyé à `pw-record` sans attendre la
libération d'un worker STT; la capture s'arrête dans le délai de grâce habituel;
la finalisation peut attendre les workers mais n'allonge pas la prise de son;
aucun segment n'est réordonné ou dupliqué.

**Vérification:** bloquer les workers, produire plus de segments que la capacité
des files, demander `stop`, puis vérifier l'ordre des signaux et du PCM sous
`go test -race ./internal/audio/pwrecord ./internal/stt ./internal/session`.

### MUR-004 - Préserver la mort spontanée de `pw-record` dans `Stop`

**Statut (31 juillet 2026): terminé.** `waitProcess` et `Stop` s'arbitrent sous
le même mutex. Une sortie observée en `actionNone` fige `audio_failed` et
n'expose aucun WAV; seul un stop passé en `actionStopping` avant la sortie peut
finaliser normalement. Les tests couvrent exit 0/non-zéro, 100 courses par
exécution et une session garantissant zéro appel STT/sink. Au jalon P2, ce test
a été resynchronisé sur l'observation effective de `Done` afin de ne pas refuser
le cas légitime où Stop gagne; 500 scénarios mort-gagnante et 500 courses libres
passent sous race. Vérifications passées: `go test -race
./internal/audio/pwrecord ./internal/session`, quatre suites globales sous race
et `git diff --check`.

**Constat:** `waitProcess` mémorise correctement une mort spontanée dans
`doneErr`. Si `Stop` est appelé après cette mort, `begin` passe néanmoins à
`actionStopping`, `terminate` considère le processus déjà terminé comme un arrêt
gracieux et `Stop` ne relit ni `doneErr` ni `procErr`. Un WAV partiel mais valide
peut alors être transcrit et livré comme succès.

**Références:** `internal/audio/pwrecord/pwrecord.go:121-129`,
`internal/audio/pwrecord/pwrecord.go:188-225`,
`internal/audio/pwrecord/pwrecord.go:255-303`, `docs/audio.md:25-32`.

**Travail attendu:** définir un arbitrage atomique entre la demande de stop et la
sortie du processus. Une sortie observée alors que l'action était encore
`actionNone` doit rester un `audio_failed`; seul un stop pris en charge avant la
sortie peut produire une fin normale.

**Critères d'acceptation:** appeler `Stop` après `Done` retourne `audio_failed` et
n'expose aucun WAV au STT; un stop réellement demandé avant la sortie reste
valide; le résultat est déterministe en cas de course stop/sortie.

**Vérification:** ajouter des tests de sortie avec code zéro et non zéro avant
`Stop`, puis un stress test de la course sous `go test -race
./internal/audio/pwrecord ./internal/session`.

### MUR-005 - Faire terminer `dictate` à la mort du recorder

**Statut (31 juillet 2026): terminé.** Après readiness, `dictate` attend aussi
`recording.Done()`. Une mort spontanée propage `audio_failed`, annule la lecture
stdin quand elle expose `CancelRead` (ou ferme `os.Stdin`, possédé par le
binaire), appelle `Abort` et interdit Stop, STT, pipeline et sink. EOF et signaux
restent des arrêts normaux. Les tests processus utilisent un faux `pw-record`
ready puis défaillant avec stdin bloqué et vérifient délai, nettoyage et absence
de provider/sink. Vérifications passées: `go test -race ./internal/cli
./internal/acceptance`, stress ciblés sous race, `go vet` des packages concernés,
`go test ./...` et `git diff --check`.

**Constat:** après readiness, `dictate` attend uniquement l'annulation du contexte
ou EOF sur stdin. Il n'observe plus `recording.Done()`. Si PipeWire ou
`pw-record` meurt pendant qu'un stdin reste ouvert, la commande bloque
indéfiniment et peut ensuite produire un faux succès via le défaut de `Stop`.

**Références:** `internal/cli/composable.go:179-223`,
`internal/audio/pwrecord/pwrecord.go:111-129`, `docs/audio.md:25-28`.

**Travail attendu:** intégrer `recording.Done()` dans l'attente de capture et
propager immédiatement `recording.Err()` comme `audio_failed`. Le chemin doit
annuler le lecteur stdin si possible, nettoyer la capture et interdire STT et
sink.

**Critères d'acceptation:** une mort du recorder après readiness termine la
commande dans un délai borné avec code 1 et classe `audio_failed`; aucun provider
ni sink n'est appelé; EOF et signal continuent de déclencher un arrêt normal.

**Vérification:** faux `pw-record` qui devient ready puis sort pendant que stdin
reste bloqué, suivi de `go test -race ./internal/cli ./internal/acceptance`.

## P1 - Correction et durcissement prioritaires

### MUR-006 - Drainer l'état UI terminal avant fermeture

**Statut (31 juillet 2026): terminé.** Les événements terminaux sont prioritaires
et drainés avant fermeture. L'OSD reçoit le terminal, voit stdin fermé, attend
son timer puis sort naturellement; un kill suivi de `Wait` borne les helpers
défaillants, tandis qu'une fermeture sans terminal reste rapide. Le helper GTK
attend désormais le timer terminal après EOF. Vérifications passées: `go test
-race ./internal/ui ./internal/session`, stress ciblés sous race, tests unitaires
Python du helper, `go vet` des packages concernés, `go test ./...` et
`git diff --check`.

**Constat:** `Orchestrator.Run` appelle `UI.Close` juste après la publication du
résultat terminal. `Notify.Close` peut sélectionner `stop` avant la notification
encore en file. `OSD.Close` annule et tue immédiatement le helper. Le helper GTK
quitte en outre sur EOF sans attendre le timer terminal. Les notifications de
fin peuvent disparaître et les délais OSD configurés sont inopérants.

**Références:** `internal/session/orchestrator.go:152-169`,
`internal/ui/notify.go:93-121`, `internal/ui/osd.go:107-180`,
`helpers/osd-gtk/murmur-osd.py:167-220`.

**Travail attendu:** définir une fermeture drainante et bornée pour les UI.
Garantir la remise du dernier événement terminal, fermer proprement stdin, et
laisser le helper afficher le terminal pendant son délai avant de quitter. Une
fermeture sans terminal doit rester rapide.

**Critères d'acceptation:** `Show(terminal)` suivi immédiatement de `Close`
remet toujours le terminal; le helper OSD reste visible jusqu'au timeout; aucun
processus ni goroutine ne subsiste après le délai de fermeture.

**Vérification:** tests synchronisés de notification et de helper avec délais
courts sous `go test -race ./internal/ui ./internal/session`.

### MUR-007 - Borner toutes les commandes externes de livraison

**Statut (31 juillet 2026): terminé.** Un budget partagé fixe de 5 secondes
borne les copies brute/finale, le collage et ses fallbacks, la frappe et
l'effacement transient; `dictate` utilise aussi un contexte frais borné. Un
timeout tue et récolte le groupe processus et produit `failed/sink_failed`.
Les tests avec helpers bloqués couvrent descendants, libération du verrou et du
socket, puis démarrage d'une nouvelle session. Vérifications passées: `go test
-race ./internal/clipboard ./internal/inject ./internal/sink ./internal/session
./internal/cli`, stress ciblés 20 fois sous race, `go test ./...` et
`git diff --check`.

**Constat:** `wl-copy` et `wtype` utilisent le contexte de session sans deadline.
Une fois l'état `delivering` atteint, l'annulation IPC est refusée. Un helper
bloqué peut donc conserver indéfiniment le verrou, le socket et le processus de
session. `dictate` utilise même `context.Background()` pour sa livraison.

**Références:** `internal/session/orchestrator.go:441-452`,
`internal/cli/composable.go:223-229`, `internal/clipboard/clipboard.go:37-64`,
`internal/inject/inject.go:46-63`, `internal/session/machine.go:196-205`.

**Travail attendu:** définir et documenter un budget de livraison, puis
l'appliquer à `CopyRaw`, `CopyFinal`, au collage, à la frappe et à l'effacement
transient. Le timeout doit tuer le groupe de processus et produire un résultat
terminal exploitable.

**Critères d'acceptation:** un `wl-copy` ou `wtype` bloqué termine en
`failed/sink_failed` dans le délai; verrou et socket sont libérés; une nouvelle
session peut démarrer; aucune goroutine d'attente ne fuit.

**Vérification:** exécutables factices qui ignorent stdin et les signaux usuels,
puis `go test -race ./internal/clipboard ./internal/inject ./internal/sink
./internal/session ./internal/cli`.

### MUR-008 - Valider l'intégrité de `config.toml` avant son interprétation

**Statut (31 juillet 2026): terminé, politique symlink révisée à la demande de
l'utilisateur.** Les symlinks finaux et intermédiaires de configuration sont
acceptés pour le workflow dotfiles. La cible effectivement ouverte doit rester
régulière, appartenir à l'EUID et ne pas être inscriptible par groupe/autres. Un
snapshot borné à 1 Mio est lu avant décodage; device/inode/taille/mtime/ctime et
une réouverture détectent aussi bien réécriture in-place que remplacement ou
retargeting. Les symlinks de credentials restent refusés.

**Constat:** le répertoire de configuration est contrôlé, mais `toml.DecodeFile`
suit directement le fichier final sans vérifier son type, son propriétaire ou
ses permissions. Un `config.toml` modifiable par le groupe ou les autres peut
injecter `api_key_command`, `ui.osd.helper`, des endpoints ou un sink fichier et
obtenir une exécution sous l'identité de l'utilisateur.

**Références:** `internal/config/load.go:28-48`, `internal/xdg/xdg.go:14-71`,
`internal/config/secrets.go:94-109`, `internal/session/factory.go:50-82`.

**Travail attendu:** ouvrir la configuration par une primitive sûre et décoder
depuis ce descripteur. Vérifier au minimum fichier régulier, propriétaire égal à
l'EUID, absence d'écriture groupe/autres et stabilité de l'inode. Définir la
politique des symlinks finaux et intermédiaires, y compris pour un `ConfigPath`
explicite.

**Critères d'acceptation:** les fichiers world-writable, appartenant à un autre
UID ou substitués pendant l'ouverture sont refusés avant commande et réseau; un
fichier utilisateur `0600` ou `0644` reste accepté; les exemples suivis dans le
dépôt restent chargeables par les tests.

**Vérification:** tests de permissions, ownership, symlinks et remplacement
concurrent sous `go test -race ./internal/config ./internal/cli`.

### MUR-009 - Définir une politique sûre pour l'injection de texte distant

**Statut (31 juillet 2026): terminé.** Le défaut actif est
`type_newlines="space"`; conserver les LF exige l'opt-in explicite
`dangerous-keep` et l'ancien `keep` est refusé. Tous les chemins actifs
(`wl-copy` de paste, `wtype`, fallback et copie brute active) normalisent CR,
remplacent tabs, retirent C0/DEL/C1 et réparent l'UTF-8; seuls les LF peuvent
survivre à l'opt-in. Clipboard passif, clipboard-only, stdout et file restent
inchangés. Vérifications passées: `go test -race ./internal/inject
./internal/sink ./internal/acceptance`, config/CLI sous race, stress inject 50
fois et processus 10 fois après correction de deux TOCTOU de test,
`go test ./...` et `git diff --check`.

**Constat:** le résultat d'un provider STT ou cleanup peut être envoyé à
`wl-copy`, puis collé, ou transmis à `wtype -`. Le défaut
`sink.type_newlines="keep"` conserve CR/LF et aucun filtre global ne retire les
contrôles C0/C1 ou ESC. Un provider compromis peut ainsi provoquer une validation
dans l'application focalisée, notamment un terminal, plutôt que seulement
produire du texte.

**Références:** `internal/config/schema.go:159-164`,
`internal/stt/openai.go:168-178`, `internal/cleanup/cleanup.go:140-151`,
`internal/clipboard/clipboard.go:61-64`, `internal/inject/inject.go:46-89`,
`docs/sinks.md:43-67`.

**Travail attendu:** formaliser la frontière de confiance du texte provider.
Retirer les contrôles non textuels avant toute injection active et choisir un
défaut qui ne génère pas de touche de validation. Si les retours à la ligne
doivent rester injectables, exiger un opt-in explicitement dangereux. Ne pas
altérer les sinks passifs `stdout`, `file` et `clipboard-only` sans décision
documentée.

**Critères d'acceptation:** le défaut n'émet ni Enter, CR, LF, ESC ni contrôle;
les modes actifs et leur fallback appliquent la même politique; une conservation
des retours à la ligne requiert une configuration explicite; la documentation
explique la différence entre sink passif et injection active.

**Vérification:** provider factice renvoyant CR/LF, ESC et autres contrôles, avec
assertions sur les octets passés à `wl-copy` et `wtype`, puis `go test -race
./internal/inject ./internal/sink ./internal/acceptance`.

### MUR-010 - Durcir le répertoire de rétention audio

**Statut (31 juillet 2026): terminé.** Le store privé est parcouru/créé avec
`openat`/`mkdirat` et `O_NOFOLLOW`, exige base absolue, EUID courant et modes
sûrs, sans `chmod` d'un chemin existant. La transaction est sérialisée entre
processus par `flock`; le fichier courant est exclu des victimes. La source est
ouverte une fois sans suivre le symlink final, validée par `fstat` et copiée avec
une borne stricte. Création `0600`, inventaire, rotation et suppression restent
ancrés au descripteur `0700`, revérifié avant suppression de la source et retour.
La suppression compare en plus le device/inode du chemin à celui du fd source;
un remplacement concurrent reste intact et fait échouer la rétention.
Vérifications passées:
`go test -race ./internal/audio ./internal/xdg`, stress sécurité 20 fois,
compilation tests linux/arm64, `go test ./...` et `git diff --check`.

**Constat:** la rétention dérive son chemin de `XDG_STATE_HOME`, puis utilise
`MkdirAll`, `Chmod` et `CreateTemp`. Un symlink final existant est suivi et sa
cible est chmodée en `0700`; un `XDG_STATE_HOME` relatif est également accepté.
Des WAV sensibles peuvent donc être déposés ailleurs que dans le store annoncé.

**Références:** `internal/audio/retention.go:34-58`,
`internal/audio/retention.go:61-99`, `internal/audio/retention.go:106-137`,
`README.md:354-357`, `docs/audio.md:68-72`.

**Travail attendu:** appliquer au store de rétention un contrat de chemin privé:
base absolue, répertoire réel, propriétaire courant, permissions contrôlées et
aucun `chmod` avant validation. Utiliser autant que possible des opérations
ancrées sur des descripteurs pour éviter la substitution concurrente.

**Critères d'acceptation:** symlink, répertoire d'un autre UID, répertoire
group-writable et base relative sont refusés sans modifier la cible; tout fichier
conservé reste sous le répertoire validé en `0600`.

**Vérification:** tests de symlink et permissions, avec mode de la cible vérifié
avant et après, puis `go test -race ./internal/audio ./internal/xdg`.

### MUR-011 - Rendre `raw-and-clean` cohérent avec l'annulation

**Statut (31 juillet 2026): terminé.** `CopyRaw` a été déplacé après la
transition atomique vers `delivering`, donc aucun clipboard n'est touché sur
annulation, `no_speech` ou erreur de processing. Le succès conserve l'ordre
brut puis final; une panne de copie devient `failed/sink_failed`, et aucune
purge mensongère d'un historique persistant n'est tentée. Vérifications passées:
`go test -race ./internal/session ./internal/clipboard`, tests ciblés 30 fois,
`go test -race ./internal/sink ./internal/acceptance ./internal/cli`,
`go test ./...` et `git diff --check`.

**Constat:** `CopyRaw` est exécuté en état `processing`, alors que `cancel` reste
autorisé. Une annulation pendant cleanup, une sortie finale vide ou une erreur de
pipeline peut donc laisser le transcript brut dans un presse-papiers persistant,
tout en terminant avec un message « aucun texte n'a été livré ».

**Références:** `internal/session/orchestrator.go:394-448`,
`internal/session/orchestrator.go:521-532`, `internal/clipboard/clipboard.go:37-50`,
`internal/ui/notify.go:173-195`, `docs/sinks.md:18-41`.

**Travail attendu:** choisir et documenter un contrat unique. Option préférée:
ne produire aucun effet clipboard avant la fermeture atomique de la fenêtre
d'annulation. Alternative acceptable: suivre l'état « brut copié », tenter un
effacement avec un contexte neuf et borné sur toute sortie non livrée, et cesser
d'affirmer qu'aucun texte n'a été exposé.

**Critères d'acceptation:** le comportement de `cancelled`, `no_speech` et des
échecs de processing est explicite et testé; aucun historique persistant n'est
présenté comme effaçable; le succès nominal conserve l'ordre brut puis final si
ce mode reste supporté.

**Vérification:** processeur bloqué puis annulé, processeur vide et processeur en
erreur, avec assertions sur les appels clipboard, sous `go test -race
./internal/session ./internal/clipboard`.

### MUR-012 - Borner le volume cumulé des transcriptions segmentées

**Statut (31 juillet 2026): terminé.** `stt.max_text_bytes` borne à 16 Mio par
défaut (64 Mio maximum) tous les octets UTF-8, séparateurs, réponses principales
et fallbacks inclus; le dépassement `server_error` annule workers/requêtes et
interdit tout résultat partiel. `stt.max_segments` borne les maps, slices et
fichiers à 4096 par défaut (65536 maximum), et la concurrence en vol est limitée
par le budget texte. Vérifications passées: `go test -race ./internal/stt
./internal/session`, tests config/core/audio/WAV, `go test ./...` et
`git diff --check`.

**Constat:** chaque réponse STT est bornée à 4 MiB, mais aucun budget global ne
borne une session. En chunking live, un provider peut renvoyer presque 4 MiB par
segment; tous les résultats restent en mémoire dans les maps et slices avant
assemblage. La consommation croît donc avec la durée de capture.

**Références:** `internal/stt/openai.go:21`, `internal/stt/openai.go:159-178`,
`internal/stt/stt.go:264-355`, `internal/stt/stt.go:471-492`,
`internal/config/validate.go:20-27`.

**Travail attendu:** ajouter un budget global de texte et, si nécessaire, un
nombre maximal de segments ou une durée maximale de session. Le dépassement doit
annuler les workers restants et empêcher la livraison partielle par défaut.

**Critères d'acceptation:** la mémoire de transcript est bornée indépendamment du
nombre de segments; le dépassement produit une classe et un message stables;
batch, live et fallback partagent la même limite; aucune requête inutile ne reste
active après dépassement.

**Vérification:** plusieurs réponses individuellement valides mais cumulativement
trop volumineuses sous `go test -race ./internal/stt ./internal/session`.

### MUR-013 - Compléter la validation déterministe de configuration

**Statut (31 juillet 2026): terminé.** `config.Validate` refuse désormais modèles
et helpers vides, raccourcis inconnus, clipboard sans mode persistant et les
autres erreurs déterministes auditées avant fork/runtime/credentials/UI/capture.
Les durées sont bornées à 24 h, le journal à 1 Gio et les plafonds numériques
sont validés sans overflow; `doctor` partage le verdict et les constructeurs
restent défensifs. Vérifications passées: `go test -race ./internal/config
./internal/doctor ./internal/cli ./internal/acceptance ./internal/core
./internal/session ./internal/sink ./internal/inject`, `go test ./...` et
`git diff --check`.

**Constat:** `config.Validate` ne rejette pas un modèle provider vide, un
raccourci de collage inconnu, ni `sink.mode="clipboard"` combiné à
`clipboard_mode="none"|"transient"`. Les deux derniers cas sont rejetés plus
tard dans les constructeurs, parfois après le handshake réussi de `start`; le
modèle vide atteint quant à lui la requête HTTP. Les durées positives ne
possèdent par ailleurs pas de borne haute et peuvent déborder lors de leur
conversion en `time.Duration`.

**Références:** `internal/config/validate.go:13-140`,
`internal/config/validate.go:166-220`, `internal/sink/sink.go:54-89`,
`internal/inject/inject.go:28-43`, `internal/core/core.go:78-81`,
`internal/session/factory.go:94-103`, `internal/cli/start_unix.go:29-136`.

**Travail attendu:** faire de `config.Validate` le préflight complet annoncé:
modèles non vides, raccourcis connus, combinaisons sink/clipboard valides et
bornes pratiques empêchant tout overflow. Conserver les validations défensives
dans les constructeurs.

**Critères d'acceptation:** toute erreur déterministe est refusée par le parent
avant fork, socket, credential helper ou capture; `doctor` rend le même verdict;
les valeurs maximales sont documentées et testées aux frontières.

**Vérification:** tests table-driven dans `internal/config`, puis tests de
processus garantissant l'absence de helper et de capture sous `go test -race
./internal/config ./internal/doctor ./internal/cli ./internal/acceptance`.

### MUR-014 - Imposer une deadline à chaque check externe de `doctor`

**Statut (31 juillet 2026): terminé.** Chaque invocation externe (`pw-cli`,
health HTTP, ping D-Bus et helper OSD) reçoit un timeout local de 5 secondes;
une expiration échoue le check mais n'empêche pas les checks indépendants, et
l'annulation globale reste prioritaire. Les groupes et descendants sont tués et
récoltés par `sysexec`; le budget séquentiel total est documenté. Vérifications
passées: `go test -race ./internal/doctor ./internal/sysexec`, tests ciblés 20
fois sous race, `go test ./...` et `git diff --check`.

**Constat:** le health HTTP est plafonné à cinq secondes, mais `pw-cli`, le helper
OSD et le ping D-Bus utilisent le contexte global sans deadline locale. Un outil
ou service bloqué peut empêcher `doctor` de rendre son rapport.

**Références:** `internal/doctor/doctor.go:47-118`,
`internal/doctor/doctor.go:206-286`, `internal/ui/notify.go:226-235`.

**Travail attendu:** appliquer un timeout par check externe, tuer le groupe de
processus expiré et continuer les checks indépendants afin de produire un rapport
complet. Documenter le budget total attendu.

**Critères d'acceptation:** source PipeWire, helper OSD ou D-Bus bloqué donne un
check `failed` dans un délai borné; les checks suivants sont exécutés; aucun
processus enfant ne subsiste.

**Vérification:** fakes bloquants pour chaque dépendance sous `go test -race
./internal/doctor ./internal/sysexec`.

### MUR-015 - Conserver une provenance honnête en mode multi-provider

**Statut (31 juillet 2026): terminé.** Toute transcription utilisant plusieurs
providers expose désormais `provider="mixed"`, indépendamment de leur ordre,
tout en conservant `SegmentResult.Provider`; le mono-provider reste inchangé.
L'orchestrateur n'infère plus le provider global depuis le dernier segment et
événements, CLI composable et `last-result.json` sont alignés. Vérifications
passées: `go test -race ./internal/stt ./internal/session ./internal/cli
./internal/acceptance`, tests ciblés, `go test ./...` et `git diff --check`.

**Constat:** `stt.assemble` laisse `MultiResult.Provider` vide lorsqu'il existe
plusieurs providers. L'orchestrateur remplace ensuite cette valeur par le
provider du dernier segment. Les événements et `last-result.json` peuvent ainsi
présenter une transcription mixte comme entièrement produite par un seul
provider.

**Références:** `internal/stt/stt.go:471-492`,
`internal/session/orchestrator.go:384-391`,
`internal/session/orchestrator.go:483-492`,
`internal/session/orchestrator.go:558-569`, `docs/providers.md:42-47`.

**Travail attendu:** représenter explicitement la provenance mixte, par exemple
avec `provider="mixed"` ou une structure dédiée, sans perdre le détail par
segment. Aligner événements, résultat CLI et dernier résultat.

**Critères d'acceptation:** aucun contrat global n'attribue tout le texte au
dernier provider; le cas mono-provider reste inchangé; l'ordre des providers ne
change pas la représentation globale.

**Vérification:** tests primary/fallback dans les deux ordres sous `go test -race
./internal/stt ./internal/session ./internal/cli ./internal/acceptance`.

### MUR-016 - Corriger les événements de progression non-live

**Statut (31 juillet 2026): terminé.** Batch et live utilisent désormais le
même contrat `stt.Progress`: `started` est émis lors de la tentative réelle,
`done`/`failed` reprennent l'index original converti en base publique 1, et les
tentatives de fallback indiquent leur provider. Aucun événement n'est fabriqué
pour une tentative non lancée. Vérifications passées: `go test -race
./internal/session ./internal/stt`, CLI et acceptance sous race,
`go test ./...` et `git diff --check`.

**Constat:** le chemin non-live publie tous les `partial_started` avant le premier
appel provider, publie les échecs avec `partial=0`, puis numérote les succès par
leur position dans les résultats plutôt que par `SegmentResult.Index`. Avec une
récupération partielle, la télémétrie ne permet pas d'identifier les segments
effectivement tentés, réussis ou échoués.

**Références:** `internal/session/orchestrator.go:457-487`,
`internal/stt/stt.go:414-430`.

**Travail attendu:** utiliser le contrat `stt.Progress` aussi pour le batch ou
retourner des outcomes indexés. Émettre `started` au moment réel de la tentative
et conserver l'index original, en base cohérente avec le protocole public.

**Critères d'acceptation:** pour trois segments avec échec du deuxième, les
événements indiquent exactement `done=[1,3]` et `failed=[2]` en mode partiel;
aucun `started` n'est émis pour une tentative jamais lancée.

**Vérification:** tests d'événements avec échec intermédiaire, arrêt anticipé et
fallback sous `go test -race ./internal/session ./internal/stt`.

### MUR-017 - Préserver la classe des dégradations cleanup et plugin

**Statut (31 juillet 2026): terminé.** `textpipe.Warning` transporte classe et
composant contrôlé; cleanup et plugin préservent ainsi `server_error`, `timeout`,
`auth_failed`, `policy_denied`, `unavailable`, etc. jusqu'aux événements, JSON
CLI et dernier résultat. Causes brutes, composants non sûrs et corps HTTP ne sont
jamais sérialisés. Vérifications passées: `go test -race ./internal/textpipe
./internal/core ./internal/session ./internal/cli`, cleanup et plugins sous race,
acceptance sous race, `go test ./...` et `git diff --check`.

**Constat:** `cleanup.Result.Degraded` conserve sa classe, mais
`textpipe.Warning` ne transporte que stage et message. L'orchestrateur et les
commandes composables reclassent ensuite toutes les dégradations du pipeline en
`internal`, y compris timeout, auth, policy ou indisponibilité.

**Références:** `internal/core/core.go:90-96`,
`internal/textpipe/pipeline.go:13-20`, `internal/textpipe/pipeline.go:92-128`,
`internal/session/orchestrator.go:426-435`,
`internal/cli/composable.go:306-315`.

**Travail attendu:** enrichir `textpipe.Warning` avec au minimum classe et
composant, puis préserver la taxonomie de l'erreur typée dans tous les contrats.
Ne pas recopier de cause brute susceptible de contenir du texte sensible.

**Critères d'acceptation:** un cleanup HTTP 503 produit `server_error`; un timeout
cleanup ou plugin produit `timeout`; événements, JSON CLI et dernier résultat
restent cohérents.

**Vérification:** tests de chaque classe sous `go test -race ./internal/textpipe
./internal/core ./internal/session ./internal/cli`.

### MUR-018 - Classer le statut HTTP avant les erreurs de corps

**Statut (31 juillet 2026): terminé.** STT, health STT et cleanup classent tout
non-2xx avant de drainer au plus 4 Mio puis fermer le corps; une erreur de
lecture/fermeture ne remplace plus `auth_failed`, `timeout`, `unavailable` ou
`server_error`, et aucun corps n'est exposé. Les 2xx invalides restent
`server_error`. Vérifications passées: tests table-driven sous `go test -race
./internal/stt ./internal/cleanup`, core/textpipe sous race, test multipart rendu
déterministe puis répété 500 fois sous race, deux `go test ./...` et
`git diff --check`.

**Constat:** les clients STT et cleanup lisent et bornent le corps avant de
classifier le statut. Un HTTP 401 avec un corps supérieur à 4 MiB devient ainsi
`server_error` au lieu de `auth_failed`, en contradiction avec la table D17.

**Références:** `internal/stt/openai.go:151-166`,
`internal/cleanup/cleanup.go:125-139`, `docs/providers.md:83-93`.

**Travail attendu:** pour une réponse non-2xx, conserver le statut comme classe
principale et drainer ou fermer le corps de façon bornée sans laisser une erreur
de lecture remplacer cette classe. Le corps ne doit jamais apparaître dans
l'erreur.

**Critères d'acceptation:** 401/403 restent `auth_failed`, 408 `timeout`, 429
`unavailable` et 5xx `server_error`, même avec corps surdimensionné ou illisible.

**Vérification:** tests table-driven des statuts et comportements de corps sous
`go test -race ./internal/stt ./internal/cleanup`.

### MUR-019 - Rendre le journal récupérable et ses erreurs observables

**Statut (31 juillet 2026): terminé.** Le journal mémorise sa première erreur via
`Err`/`Close`, conserve ou restaure un fichier actif après échec de rotation et
retente les écritures. Un callback indépendant, unique et sans récursion publie
un warning borné `internal/session-journal` visible par `status`; chemins et
causes détaillés restent locaux, sans transcript, prompt, corps ou secret.
Vérifications passées: injections `Stat`/`Rename`/`Write`/réouverture sous
`go test -race ./internal/events ./internal/session ./internal/cli`, acceptance,
`go test ./...` et `git diff --check`.

**Constat:** les erreurs de marshal, rotation et écriture sont ignorées. Une
rotation peut fermer le fichier puis échouer pendant un renommage, laissant
`j.file` définitivement fermé. Tous les événements suivants sont alors perdus
silencieusement et le `defer journal.Close()` du processus ignore aussi l'erreur
finale.

**Références:** `internal/events/journal.go:46-99`,
`internal/cli/start_unix.go:207-217`.

**Travail attendu:** mémoriser et exposer la première erreur, conserver ou
rouvrir un fichier actif après rotation échouée, et acheminer la dégradation vers
un canal indépendant du journal. Préserver l'interdiction de transcript, prompt,
corps HTTP et secret.

**Critères d'acceptation:** un échec transitoire de rotation ne tue pas
silencieusement toutes les écritures suivantes; `Close` retourne une erreur
utile; l'échec est visible dans le résultat ou un log borné.

**Vérification:** injecter les erreurs `Stat`, `Rename`, `Write` et réouverture
sous `go test -race ./internal/events ./internal/session ./internal/cli`.

### MUR-020 - Empêcher l'affichage d'un ancien résultat après échec de persistance

**Statut (31 juillet 2026): terminé.** Un marqueur atomique
`last-result.session.json` passe à `pending=true` avant exposition IPC et n'est
commité qu'après la persistance terminale. Crash, ENOSPC, permission ou renommage
raté rendent donc l'ancien résultat explicitement périmé; `status --json` expose
`last_result_stale=true` sans ancien `last_result`. Le résultat métier reste
inchangé, l'échec est opérationnel. Vérifications passées: erreurs writer/rename
sous `go test -race ./internal/session ./internal/cli`, acceptance,
`go test ./...` et `git diff --check`.

**Constat:** si `WriteLastResult` échoue, l'ancien `last-result.json` reste en
place. Après disparition du socket, `status` peut le présenter comme dernier
résultat sans signaler qu'une session plus récente n'a pas été persistée.

**Références:** `internal/session/orchestrator.go:152-169`,
`internal/session/result.go:32-69`, `internal/cli/cli.go:192-220`.

**Travail attendu:** rendre l'échec de persistance distinguable d'une absence de
nouvelle session et empêcher un résultat antérieur d'être présenté sans marqueur
de péremption. Conserver l'écriture atomique.

**Critères d'acceptation:** après une nouvelle session non persistable, `status`
ne présente pas silencieusement l'ancien `session_id`; le diagnostic reste
possible sans fichier partiellement écrit.

**Vérification:** précharger un résultat, forcer ENOSPC ou une erreur de writer,
puis tester `status` sous `go test -race ./internal/session ./internal/cli`.

### MUR-021 - Ne pas supprimer un `murmur-osd` étranger à l'installation

**Statut (31 juillet 2026): terminé.** `uninstall` ne retire `murmur-osd` que si
`INSTALL_OSD=1` est explicitement répété, comme les variables de chemins. Le
contrat est documenté; `installcheck` couvre une sentinelle avec opt-out, le
helper réellement installé avec opt-in et la préservation des autres fichiers
de `BINDIR`. Vérifications passées: `make installcheck` et
`git diff --check -- Makefile README.md`.

**Constat:** `make uninstall` supprime toujours `$(BINDIR)/murmur-osd`, même si
`INSTALL_OSD=0` et si ce fichier existait avant l'installation. Dans un préfixe
partagé, la désinstallation peut donc détruire un helper non possédé par Murmur.

**Références:** `Makefile:52-68`, `README.md:361-368`.

**Travail attendu:** rendre install et uninstall symétriques. Ne supprimer le
helper que sur demande explicite ou à partir d'un manifeste prouvant qu'il a été
installé par cette copie. Documenter les variables à réutiliser.

**Critères d'acceptation:** une sentinelle `murmur-osd` survit à une installation
avec `INSTALL_OSD=0` puis désinstallation; le helper réellement installé avec
`INSTALL_OSD=1` est supprimé; aucun autre fichier de `BINDIR` n'est touché.

**Vérification:** étendre `make installcheck` avec les deux scénarios.

## P2 - Robustesse, contrats et exploitation

### MUR-022 - Éviter l'effacement transient avant consommation du collage

**Statut (31 juillet 2026): terminé.** Toute combinaison effective
`transient+paste` avec raccourci de collage, y compris `auto_mode="paste"`, est
refusée au préflight et défensivement par le sink: `wtype` ne confirme pas la
consommation Wayland. Les chemins sans collage réel restent acceptés et leur
nettoyage transient est conservé sur succès comme échec; les alternatives et
limites de confidentialité sont documentées. Vérifications passées: `go test
-race ./internal/config ./internal/sink ./internal/session ./internal/cli`,
acceptance, `go test ./...` et `git diff --check`.

**Constat:** après le retour réussi de `wtype`, le sink lance immédiatement
`wl-copy --clear`. Ce succès garantit seulement l'émission du raccourci, pas que
l'application cible ait traité l'événement et demandé la sélection Wayland. Le
collage transient peut donc être vide ou reprendre une ancienne sélection.

**Références:** `internal/sink/sink.go:130-136`,
`internal/inject/inject.go:55-63`, `docs/sinks.md:26-32`,
`docs/sinks.md:53-56`.

**Travail attendu:** définir un contrat réalisable pour `transient+paste`:
interdire la combinaison, ajouter une grâce explicite et bornée, ou utiliser un
mécanisme confirmant la consommation. Ne pas présenter le best-effort comme une
garantie de confidentialité.

**Critères d'acceptation:** soit `transient+paste` est refusé avant capture, soit
une grâce bornée et documentée réduit la course sans prétendre garantir un
consommateur arbitrairement lent; l'échec du sink tente toujours le nettoyage
prévu; les limites de confidentialité restent explicites.

**Vérification:** test d'intégration Wayland répétable avec consommateur retardé,
complété par les tests unitaires de l'ordre des appels.

### MUR-023 - Faire fermer les abonnements actifs par `ipc.Server.Close`

**Statut (31 juillet 2026): terminé.** `Server.Close`, idempotent, ferme le
listener et toutes les connexions actives puis annule un contexte interne partagé
par abonnements et handlers ordinaires. `Serve` retourne sans annulation externe,
les clients abonnés observent EOF et le serveur est explicitement one-shot.
Vérifications passées: `go test -race ./internal/ipc`, course ciblée 100 fois,
session et CLI sous race, `go test ./...` et `git diff --check`.

**Constat:** `Server.Close` ferme uniquement le listener. `Serve` attend ensuite
les handlers, mais un abonnement utilise le contexte fourni à `Serve` et garde sa
connexion ouverte. Un appelant qui fait `Close` sans annuler ce contexte peut
bloquer indéfiniment.

**Références:** `internal/ipc/server.go:19-26`,
`internal/ipc/server.go:48-79`, `internal/ipc/server.go:111-142`.

**Travail attendu:** donner au serveur un contexte interne annulé par `Close` ou
suivre et fermer les connexions actives. Rendre `Close` idempotent et sûr face à
une annulation concurrente.

**Critères d'acceptation:** `Close` fait retourner `Serve` dans un délai borné
avec un subscriber actif; le client observe la fermeture du flux; aucune
goroutine ne reste bloquée.

**Vérification:** test `Close` avec abonnement actif et course `Close`/cancel sous
`go test -race ./internal/ipc`.

### MUR-024 - Propager les erreurs du flux `--events`

**Statut (31 juillet 2026): terminé.** L'émetteur mémorise sous mutex la première
erreur ou écriture courte puis abandonne les suivantes. Une erreur métier garde
sa classe; sinon le résultat JSON devient `internal/io` et le code vaut 1, ou le
mode non-JSON signale uniquement le code, sans texte humain dans stderr JSONL.
La troncature partielle inévitable du dernier JSON est documentée. Vérifications
passées: tests writers ciblés, `go test -race ./internal/cli`, acceptance,
`go test ./...` et `git diff --check`.

**Constat:** `commandEmitter.emit` ignore le retour de `events.EncodeJSONL`.
EPIPE ou une erreur du writer peut donc tronquer le flux demandé tout en laissant
la commande retourner un succès.

**Références:** `internal/cli/composable.go:343-382`,
`internal/cli/composable.go:415-437`, `docs/cli.md:156-167`.

**Travail attendu:** mémoriser la première erreur de l'émetteur et la prendre en
compte dans `finishCommand`, sans mélanger de texte humain au JSONL. Définir la
priorité entre une erreur métier et une erreur d'écriture du canal d'événements.

**Critères d'acceptation:** un writer qui échoue après N événements donne un code
non nul; stdout JSON reste valide; stderr ne contient aucun mélange JSONL/texte.

**Vérification:** writer factice défaillant sous `go test -race ./internal/cli`.

### MUR-025 - Réduire l'environnement des helpers de credentials

**Statut (31 juillet 2026): terminé.** La politique commune
`CredentialHelperEnvironment` retire de chaque helper toutes les variables
`api_key_env` déclarées par les providers STT/cleanup actifs, fallback ou
inactifs, doublons inclus, en respectant la casse Unix. L'environnement usuel
non-secret reste disponible pour les agents; session, composables et `doctor`
partagent ce filtrage. Vérifications passées: `go test -race ./internal/config
./internal/core ./internal/session ./internal/doctor`, CLI et acceptance sous
race, `go test ./...` et `git diff --check`.

**Constat:** l'exécuteur des `api_key_command` reçoit l'environnement complet.
Un helper du provider B peut lire les variables `api_key_env` des providers A,
des fallbacks et des providers inactifs. Les autres commandes externes sont
filtrées, mais pas les helpers de secrets.

**Références:** `internal/session/factory.go:107-117`,
`internal/doctor/doctor.go:206-235`, `internal/config/secrets.go:135-156`.

**Travail attendu:** construire une allowlist minimale pour les helpers ou, au
minimum, retirer toutes les variables nommées par `api_key_env`. Prévoir une
configuration explicite pour les variables nécessaires à un agent de secrets.

**Critères d'acceptation:** aucun helper ne voit les credentials d'un autre
provider; les intégrations `pass`, `secret-tool` et agents usuels conservent les
variables explicitement nécessaires; session, commandes composables et `doctor`
partagent la même politique.

**Vérification:** helper qui sérialise son environnement sous `go test -race
./internal/config ./internal/core ./internal/session ./internal/doctor`.

### MUR-026 - Refuser les symlinks intermédiaires des fichiers de credentials

**Statut (31 juillet 2026): terminé.** `api_key_file` exige un chemin absolu,
parcouru depuis `/` avec `openat(O_NOFOLLOW)` sur chaque composant. Le fichier
final, ouvert non bloquant, doit être régulier, appartenir à l'EUID, sans droits
groupe/autres et inférieur à 64 Kio; il est lu sur le descripteur stable. Les
tests couvrent symlinks à chaque niveau et remplacement concurrent d'un parent.
Vérifications passées: `go test -race ./internal/config`, stress 20 fois, core,
session, doctor et CLI sous race, `go test ./...`, build linux/arm64 et
`git diff --check`.

**Constat:** `O_NOFOLLOW` protège uniquement le dernier composant de
`api_key_file`. Un parent symlinké dans un répertoire contrôlable peut rediriger
l'ouverture vers n'importe quel fichier régulier `0600` appartenant à la
victime, puis faire utiliser son contenu comme bearer.

**Références:** `internal/config/secrets.go:61-92`,
`internal/config/config_test.go:366-379`.

**Travail attendu:** résoudre le chemin composant par composant avec `openat` et
`O_NOFOLLOW`, ou `openat2` avec les contraintes adaptées. Vérifier les parents et
ancrer les chemins relatifs dans un emplacement défini.

**Critères d'acceptation:** tout symlink final ou intermédiaire est refusé; une
substitution concurrente d'un parent ne change pas le fichier lu; le contrôle
actuel du fichier final reste appliqué.

**Vérification:** symlink à chaque niveau et boucle de substitution concurrente
sous `go test -race ./internal/config`.

### MUR-027 - Donner un effet observable à `logging.level`

**Statut (31 juillet 2026): terminé.** `Debugf`/`Infof`/`Warnf`/`Errorf` ont des
seuils distincts et écrivent en session détachée dans `murmur.log`, JSONL privé
`0600`, borné et rotatif, indépendant du journal d'événements. La destination
est préparée avant détachement; les pannes OSD/D-Bus utilisent des messages
constants sans causes brutes. Symlinks/hardlinks sont refusés et les canaris
transcript, prompt, corps, secret et argv sont testés absents. Vérifications
passées: `go test -race ./internal/logging ./internal/ui ./internal/cli
./internal/acceptance`, acceptance non cachée, `go test ./...` et
`git diff --check`.

**Constat:** le package logging n'expose que `Warnf`. Les niveaux `debug` et
`info` sont donc identiques, tandis que `error` supprime même les seuls messages
disponibles. Le processus détaché redirige stderr vers `/dev/null`, ce qui rend
les warnings UI invisibles en exploitation.

**Références:** `internal/logging/logging.go:8-35`,
`internal/cli/start_unix.go:69-84`, `internal/ui/ui.go:13-20`,
`docs/configuration.md:62-65`.

**Travail attendu:** soit simplifier la configuration aux niveaux réellement
supportés, soit implémenter debug/info/warn/error et une destination privée,
bornée et rotative pour les sessions détachées. Définir des champs structurés et
une politique stricte d'absence de contenu sensible.

**Critères d'acceptation:** chaque niveau documenté a un effet testé; une panne
OSD ou D-Bus est retrouvable après une session détachée; transcript, prompt,
corps HTTP et secret canaris sont absents des logs.

**Vérification:** tests de niveaux et test de processus détaché sous `go test
-race ./internal/logging ./internal/ui ./internal/cli ./internal/acceptance`.

### MUR-028 - Renforcer le contrat JSON des commandes composables

**Statut (31 juillet 2026): terminé.** Le schéma Draft 2020-12 impose la
cohérence succès/erreur et les variantes cleanup/STT/provider mixte; une
validation Go complète les comptes, index strictement croissants et uniques,
provenance, métadonnées et assemblage. Les tests utilisent le validateur standard
`jsonschema/v6` et valident les sorties réelles de transcribe, cleanup et dictate
en succès comme en échec. Vérifications passées: `go test -race ./internal/cli`,
`go mod verify`, acceptance sous race non cachée, `go test ./...` et
`git diff --check`.

**Constat:** le schéma accepte `success=true` avec une erreur non nulle et
`success=false` avec `error=null`. Il ne garantit pas non plus la cohérence de
`segment_count`, l'unicité ou l'ordre des index. Le test de schéma actuel ne
contient que des documents positifs et utilise un validateur maison limité.

**Références:** `docs/cli-result.schema.json:7-113`,
`internal/cli/schema_test.go:12-148`.

**Travail attendu:** ajouter les contraintes conditionnelles exprimables en JSON
Schema, compléter par une validation sémantique pour les relations entre champs,
et utiliser un validateur Draft 2020-12 standard dans les tests ou la CI.

**Critères d'acceptation:** succès avec erreur, échec sans erreur, compte de
segments incohérent et index dupliqués sont rejetés; les sorties réelles de
`transcribe`, `cleanup` et `dictate` sont validées en succès et en échec.

**Vérification:** fixtures positives et négatives sous `go test -race
./internal/cli`.

### MUR-029 - Ajouter une CI bloquante et reproductible

**Statut (31 juillet 2026): terminé, sauf validation distante nécessitant un
push.** `.forgejo/workflows/ci.yml` exécute sur checkout propre, sans cache
trompeur et avec timeouts, les contrôles natifs linux/amd64 sous Go 1.23.12 et
1.26.5; un job séparé cross-build linux/arm64 ne prétend ni exécuter ni lancer le
race detector sur arm64. Actions et versions sont épinglées. Vérifications
locales passées: yamllint/PyYAML, `go mod verify`, `make lint`, `go test -race
./...`, `make build`, `make installcheck` et cross-build sous les deux Go. Les
branches volontairement cassées n'ont pas été poussées, puisqu'aucun commit ni
push n'a été demandé; leur blocage ne peut donc pas être observé côté Forgejo.

**Constat:** aucun manifeste CI n'est suivi alors que le projet s'appuie sur une
suite de tests, le détecteur de courses, des builds multi-architecture et un
contrat d'installation. Les affirmations de `implementation-status.md` ne sont
pas vérifiées automatiquement à chaque changement.

**Références:** `Makefile:28-84`, `docs/implementation-status.md:3-18`, absence
de `.forgejo/workflows/`, `.github/workflows/` et `.gitlab-ci.yml`.

**Travail attendu:** ajouter la CI adaptée au forge utilisé. Exécuter au minimum
`go mod verify`, `make lint`, `go test -race ./...`, `make build` et
`make installcheck`, avec timeouts et sans dépendance desktop ou réseau externe.
Tester Go 1.23 et une version stable courante, ainsi que linux/amd64 et
linux/arm64.

**Critères d'acceptation:** chaque contrôle bloque une fusion lorsqu'il échoue;
le pipeline part d'un checkout propre; les tests processus et packaging ne sont
pas remplacés par un résultat de cache trompeur.

**Vérification:** pousser une branche cassant volontairement, tour à tour, le
formatage, un test, la race et `installcheck`.

### MUR-030 - Ajouter et installer une licence explicite

**Statut (31 juillet 2026): terminé.** Le projet est sous licence MIT, avec texte
canonique dans `LICENSE`, attribution `Copyright (c) 2026 Mathieu Rousseau`
issue de l'auteur Git et identifiant `SPDX-License-Identifier: MIT` dans le
README. La licence est installée dans `DOCDIR`, retirée symétriquement et un
document voisin est préservé. Vérifications passées: `make installcheck`,
recherche exacte de l'identifiant et `git diff --check`. Aucun scanner SPDX
courant n'était installé, raison pour laquelle ce contrôle externe n'a pas été
exécuté.

**Constat:** le dépôt ne contient aucun fichier de licence ni identifiant SPDX.
La redistribution et le packaging sont donc juridiquement indéterminés.

**Références:** absence de `LICENSE`, `COPYING` ou équivalent; `Makefile:8-26`,
`Makefile:52-68`.

**Travail attendu:** choisir une licence, ajouter son texte et l'identifiant SPDX
approprié, puis l'inclure symétriquement dans install, uninstall et
`installcheck`.

**Critères d'acceptation:** le statut de redistribution est non ambigu; le texte
est installé dans `DOCDIR`; uninstall ne retire que le fichier installé par le
projet.

**Vérification:** `make installcheck` et un scanner SPDX courant.

## P3 - Ambiguïtés documentaires et durcissements secondaires

### MUR-031 - Aligner la documentation des retries STT sur le code

**Statut (31 juillet 2026): terminé.** La matrice normative conserve les retries
sur 408, 429, 5xx et erreur de transport seulement sans octet multipart lu; le
timeout global inclut backoff et tentatives. La documentation explicite le
risque de duplication après émission partielle ou réponse et l'absence de
garantie exactly-once. Aucun changement production n'était nécessaire.
Vérifications passées: tests table-driven des nombres d'appels et du timeout
sous `go test -race ./internal/stt`, `go test ./...` et `git diff --check`.

**Constat:** la documentation annonce les retries après 5xx ou erreur de
transport sans octet lu. L'implémentation marque également HTTP 408 et 429 comme
rejouables.

**Références:** `internal/stt/openai.go:94-111`,
`internal/stt/openai.go:159-166`, `docs/providers.md:17-22`.

**Travail attendu:** décider normativement quels statuts sont rejouables, aligner
code et documentation, et expliquer le risque de duplication même après une
réponse HTTP. Conserver le timeout global retries et backoff compris.

**Critères d'acceptation:** la matrice 401/408/429/5xx/erreur de transport est
explicite; le nombre exact d'appels est testé pour chaque cas.

**Vérification:** tests table-driven sous `go test -race ./internal/stt`.

### MUR-032 - Clarifier si `dictate --sink` est obligatoire

**Statut (31 juillet 2026): terminé.** `murmur dictate` utilise contractuellement
stdout quand `--sink` est omis; `--sink file` exige toujours `sink.file`. Usage,
aide, README, documentation et tests de parsing/processus sont alignés, et
aucune mention utilisateur ne présente plus l'option comme obligatoire.
Vérifications passées: `go test -race ./internal/cli`, acceptance ciblée,
`go test ./...` et `git diff --check`.

**Constat:** l'usage documente `murmur dictate --sink stdout|file`, mais le
parseur définit `stdout` par défaut et accepte `murmur dictate` sans option.

**Références:** `internal/cli/cli.go:37-38`,
`internal/cli/composable.go:142-150`, `docs/cli.md:12-15`,
`docs/cli.md:98-103`, `README.md:202-214`.

**Travail attendu:** choisir entre une option réellement obligatoire et un
défaut public `stdout`, puis aligner usage, README, documentation et tests.

**Critères d'acceptation:** l'omission a un comportement contractuel unique et
testé, avec code 2 si l'option devient obligatoire.

**Vérification:** test CLI dédié à `murmur dictate` sans `--sink`.

### MUR-033 - Mettre à jour l'état réel de la validation T8.4

**Statut (31 juillet 2026): terminé.** T8.4 est marquée partiellement exécutée
dans les trois documents, avec date, blocs effectivement validés, contrôles
encore ouverts et liens croisés vers compte-rendu/checklist. La recherche globale
ne laisse « non exécuté » que dans cette tâche historique et un test sans rapport;
les liens existent et `git diff --check` passe.

**Constat:** `implementation-status.md` affirme que T8.4 n'a pas été exécutée,
alors que `desktop-validation.md` et `production-migration.md` consignent une
première validation réelle le 31 juillet 2026. Le statut ne distingue pas
validation inexistante, partielle et complète.

**Références:** `docs/implementation-status.md:18-27`,
`docs/implementation-status.md:40-46`, `docs/desktop-validation.md:259-269`,
`docs/production-migration.md:22-38`.

**Travail attendu:** marquer T8.4 comme partiellement exécutée, lister les blocs
validés et ceux encore ouverts, et lier le compte-rendu daté.

**Critères d'acceptation:** toutes les mentions de T8.4 décrivent le même état et
ne présentent pas les validations manuelles restantes comme déjà couvertes.

**Vérification:** recherche globale de `T8.4`, `non exécuté` et `validation
desktop`.

### MUR-034 - Éliminer le squatting du runtime de repli dans `/tmp`

**Statut (31 juillet 2026): terminé.** Le fallback `/tmp/murmur-<uid>` est
supprimé. Sans `XDG_RUNTIME_DIR`, toutes les commandes utilisent
`/run/user/<euid>/murmur`, à condition que le parent préexiste, soit réel,
appartienne à l'EUID et soit strictement `0700`; sinon une erreur demande de
configurer `XDG_RUNTIME_DIR`. Parcours et création sont ancrés par fd avec
`openat(O_NOFOLLOW)` et validations anti-TOCTOU. Vérifications passées:
`go test -race ./internal/xdg ./internal/cli ./internal/session ./internal/ipc
./internal/acceptance`, stress runtime/CLI, `go test ./...`, build linux/arm64 et
`git diff --check`.

**Constat:** sans `XDG_RUNTIME_DIR`, le runtime est le chemin prévisible
`/tmp/murmur-<uid>`. Un autre utilisateur peut le créer en premier. Le contrôle
d'ownership le refuse correctement, mais le sticky bit empêche ensuite la
victime de le supprimer, ce qui produit un déni de service persistant.

**Références:** `internal/xdg/xdg.go:74-98`,
`internal/xdg/xdg.go:101-159`.

**Travail attendu:** choisir un repli non squattable sous un parent possédé par
l'utilisateur, ou exiger un runtime système sûr. Toutes les commandes doivent
retrouver le même chemin sans état externe fragile.

**Critères d'acceptation:** un UID tiers ne peut pas précréer le runtime de la
victime; le répertoire final reste réel, propriétaire courant et `0700`.

**Vérification:** test multi-UID ou user namespace précréant l'ancien chemin,
puis `go test -race ./internal/xdg ./internal/cli`.

### MUR-035 - Appliquer strictement le framing JSONL de l'IPC

**Statut (31 juillet 2026): terminé.** Le serveur lit sans attendre EOF une
unique trame terminée par LF, bornée à 4 Kio, puis exige exactement une valeur
JSON suivie seulement d'espaces. Dépassement, EOF prématuré, suffixe ou seconde
valeur donnent `config_invalid` sans appeler le handler; `subscribe` ferme aussi
si des octets supplémentaires suivent sa requête. Vérifications passées:
`go test -race ./internal/ipc`, framing 50 fois sous race, CLI/session/acceptance
sous race, `go test ./...` et `git diff --check`.

**Constat:** le protocole définit une requête JSON terminée par un saut de ligne,
mais le serveur utilise directement `json.Decoder.Decode`. Des déchets ou une
seconde valeur placés dans la même trame peuvent rester dans le buffer après la
première valeur valide, tandis que le handler est déjà exécuté.

**Références:** `internal/ipc/server.go:81-109`,
`internal/ipc/client.go:116-129`, `docs/ipc.md:9-13`.

**Travail attendu:** lire une trame bornée jusqu'au saut de ligne, puis décoder
exactement une valeur depuis cette trame et refuser tout suffixe non blanc. Ne pas
attendre EOF: les clients actuels attendent la réponse sans fermer leur moitié
écriture, et `subscribe` utilise ensuite la connexion pour détecter le départ du
client. Fermer la connexion après l'unique requête non-subscribe comme
aujourd'hui.

**Critères d'acceptation:** une seconde valeur ou un suffixe invalide avant le
saut de ligne produit `config_invalid` et n'exécute pas le handler; une requête
suivie d'espaces puis du saut de ligne reste valide; les clients actuels et les
abonnements ne bloquent pas en attente d'EOF.

**Vérification:** tests de framing sous `go test -race ./internal/ipc`.
