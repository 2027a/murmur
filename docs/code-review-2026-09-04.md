# Revue du code et de l'intégration desktop

Date : 4 septembre 2026. Revue initiale sur `69a2e643`, actualisée sur
`fcd52c78a765322ae60a8ab74bb30daa6f8f90c6` (HEAD au début de cette
actualisation). Périmètre : code Go, helper Python, tests, installation,
documentation et intégration avec le desktop et les dotfiles de Mathieu.

La revue a été menée sans modifier le code ni les dotfiles. Sauf mention
historique, les références de code correspondent au HEAD réexaminé. Les
constats desktop restent l'instantané local de la revue initiale : le desktop,
les dotfiles et le binaire installé n'ont pas été réinterrogés pour cette mise
à jour. Ils ne sont pas des propriétés garanties par le dépôt.

## Bilan de l'actualisation

- **Toujours ouverts et reproduits sur HEAD :** les trois P1, rétention après
  annulation, blocage live après stop et dépassement STT masqué en succès
  partiel. Le correctif de provenance STT `95a3b5a` ne corrige pas ce dernier
  chemin.
- **Résolus :** perte du buffer IPC au handshake (`6c646da`) et absence de
  tentative de clear transient après échec de copie (`985802d`). Les tests de
  régression ajoutés passent.
- **Simplification partiellement réalisée :** les deux tris STT par insertion
  ont été remplacés par `sort.Slice` (`95a3b5a`). Les autres coûts et
  abstractions signalés restent présents.
- **Contrat de livraison modifié :** seul le premier raccourci de collage est
  émis ; son échec ne déclenche plus de raccourci suivant ni de frappe de
  secours. Cela ne prouve toujours pas que l'application a consommé le collage.
- **Autres constats ouverts :** ils sont conservés ci-dessous avec leurs
  références actuelles. Les fichiers de journalisation, le Makefile, le helper
  Python et les documents health/migration concernés sont inchangés depuis la
  revue initiale.
- **Vérification :** lint et suite Go complète sous `-race` passent sur HEAD,
  ainsi que les tests Python. Les reproductions supplémentaires des P1
  échouent toujours comme attendu ; elles ne font pas partie de la suite
  versionnée.

## Défauts prioritaires ouverts

Trois défauts reproduits remettent en cause des garanties explicites du projet.
Ils concernent des combinaisons d'options, pas nécessairement l'usage actuel.

### 1. P1 : une session annulée peut conserver son audio

Références : `internal/session/orchestrator.go:376-390`,
`internal/session/orchestrator.go:490-493`,
`internal/audio/recorder.go:44-51`.

Avec `audio.keep_recordings=true` ou `policy.retain_diagnostics=true`,
l'annulation après arrêt de la capture n'empêche pas sa conservation.

`Release()` applique la rétention indépendamment du résultat terminal. Pendant
le processing, la copie diagnostique existe même déjà avant l'annulation.

Reproduction : capture batch et live, annulation pendant transcription et
pendant processing. Dans les quatre cas :

```text
résultat = cancelled
appels sink = 0
WAV conservés = 1
```

Chaque WAV conservé contient les 64 044 octets de la capture synthétique de deux
secondes. Les quatre scénarios ont échoué lors du premier essai puis de trois
répétitions sous `-race`. Les contrôles avec rétention désactivée passent dans
les quatre cas, sur dix répétitions sous `-race`. Sur le HEAD réexaminé, les
quatre défauts sont à nouveau reproduits une fois et les quatre contrôles
passent à nouveau dix fois sous `-race`.

Cela contredit la garantie « une annulation ne conserve jamais la capture » de
`README.md:487`.

Correction minimale : différer la décision de rétention jusqu'à la fermeture de
la fenêtre d'annulation ou au résultat terminal. Un simple test de `ctx.Err()`
autour de l'appel actuel laisserait une course et ne couvrirait pas
l'annulation pendant le processing.

Dans le TOML desktop examiné, les options de rétention sont désactivées ; ce
défaut n'y entraîne donc pas de conservation.

### 2. P1 : le chunking live peut rester bloqué après stop

Références : `internal/session/orchestrator.go:301-305`,
`internal/session/orchestrator.go:354-377`,
`internal/audio/pwrecord/live.go:174-180`.

Après `stop`, l'orchestrateur attend la finalisation de la capture sans continuer
à surveiller le résultat STT. Si celui-ci termine prématurément sur dépassement
de budget, il cesse de consommer les segments. Le forwarder et le recorder
peuvent alors rester bloqués.

Reproduction : un worker, plusieurs segments en attente, puis réponse dépassant
`max_text_bytes` après réception de `SIGINT`. La chaîne STT retourne son erreur,
mais la session reste en `stopping`. Les piles confirment les attentes :

```text
Orchestrator.runLive
  internal/session/orchestrator.go:354
  -> liveRecording.Stop
    internal/audio/pwrecord/live.go:223

Orchestrator.runLive.func1
  internal/session/orchestrator.go:301
  -> forwarder bloqué sur le transfert du segment

liveRecording.emit
  internal/audio/pwrecord/live.go:176
  -> émission bloquée
```

Reproduit lors du premier essai puis de trois répétitions sous `-race`. Le même
scénario avec un budget suffisant termine `completed` et livre exactement une
fois, sur dix répétitions. Sur le HEAD réexaminé, le blocage est à nouveau
reproduit une fois et le contrôle dans le budget passe dix fois sous `-race`.

Conséquences : pas de résultat final, session toujours occupée ; un `cancel`
supplémentaire débloque la situation mais remplace le résultat attendu par
`cancelled`. Le microphone est bien arrêté. La variante `max_text_bytes` est
reproduite ; la variante `max_segments` n'a pas été exécutée séparément.

Correction minimale : surveiller simultanément l'arrêt de capture et la
terminaison STT pendant toute la finalisation ; annuler les producteurs dès que
le consommateur termine prématurément, en préservant l'erreur de limite.

Dans le TOML desktop examiné, `live_chunking` est désactivé.

### 3. P1 : un dépassement STT peut devenir un succès partiel

Références : `internal/stt/stt.go:583-600`, `internal/stt/stt.go:273-275`.

En batch avec `allow_partial=true`, une première erreur ordinaire masque une
erreur de budget survenue ensuite. `transcribeWith` garde `firstErr`, puis
l'appelant transforme cette erreur ordinaire en récupération partielle.

Reproduction sans fallback, avec un budget de huit octets :

```text
segment 0 -> unavailable
segment 1 -> "aaaa"
segment 2 -> "bbbb", dépassement avec le séparateur

résultat -> error=nil, text="aaaa", partial=true
```

Le contrôle sans première erreur retourne correctement une erreur de limite.
Le dépassement est détecté, mais son caractère fatal est perdu. Ce n'est pas une
allocation illimitée : c'est la garantie de non-livraison après dépassement qui
est contournée. La reproduction s'arrête au résultat de `TranscribeSegments` ;
le passage jusqu'au sink est établi par lecture du pipeline de session, pas par
une livraison réelle. Le même défaut et son contrôle sont à nouveau reproduits
sur HEAD sous `-race`. La correction de provenance et de tri dans `95a3b5a`
laisse intacte la conservation de `firstErr` sur ce chemin.

Correction minimale : faire primer immédiatement les erreurs de limite et
d'annulation sur la première erreur récupérable.

## Autres défauts ouverts

Sauf indication contraire, ces constats sont établis par lecture des chemins
d'exécution, sans reproduction de leur effet sur le desktop.

### Concurrence et texte

- **P2 : écriture non protégée de `runResult`.**
  `internal/session/orchestrator.go:166` affecte la structure hors de
  `metadataMu`, alors que `currentProvider()` la lit sous ce mutex à
  `:607-610`. Un handler `cancel` déjà engagé peut croiser cette affectation.
  Course identifiée statiquement, non déclenchée sous `-race`. Calculer le
  résultat localement, puis l'affecter sous verrou, sans conserver le verrou
  pendant l'appel à `finalResult`. Le test concurrent existant passe dix
  répétitions supplémentaires, sans couvrir l'entrelacement identifié.
- **P2 : la fermeture des guillemets colle le mot suivant.**
  `internal/textpipe/render.go:37-39` supprime systématiquement l'espace après
  la commande de fermeture. Une phrase comme `ouvre les guillemets bonjour
  ferme les guillemets puis continuer` devient `« bonjour »puis continuer`.
  Même problème avec les guillemets ASCII utilisés sur le desktop examiné.
  Préserver le séparateur devant de la prose, pas devant une ponctuation à
  accoler.

### Fichiers et rétention

- **P2 : un FIFO peut bloquer l'ouverture des logs.**
  `internal/logging/logging.go:257-275` ouvre en écriture avant de vérifier le
  type. Un `murmur.log` remplacé par un FIFO sans lecteur bloque `start` alors
  que le verrou est déjà tenu. Même problème pour le journal événementiel,
  `internal/events/journal.go:69-78`. Ouvrir en non-bloquant, puis valider le
  descripteur.
- **P2 : le journal peut modifier un fichier extérieur au runtime.**
  `internal/events/journal.go:69-78` suit les symlinks et applique `chmod` ; la
  rotation mono-fichier utilise `O_TRUNC` à `:204-205`. Une entrée préexistante
  incorrecte, symlink ou hardlink, peut donc conduire à modifier les
  permissions, ajouter du JSONL ou tronquer un autre fichier. Le runtime privé
  réduit l'exposition : ce n'est pas une attaque interutilisateur démontrée.
  Le logger opérationnel possède déjà des protections absentes du journal.
- **P2 : la taille du journal n'est pas réellement bornée.**
  `internal/events/journal.go:182-185` écrit même si la rotation nécessaire a
  échoué. Une rotation durablement impossible entraîne une croissance sans
  borne. Une ligne individuellement trop grande est aussi écrite après
  rotation. Puisque la perte d'événements est déjà acceptée, abandonner
  l'écriture est plus cohérent que sacrifier le plafond de stockage.
- **P2 : réduire le nombre de rotations ne purge pas les anciennes.**
  `internal/logging/logging.go:225-245` et
  `internal/events/journal.go:204-230` ne visitent que les suffixes
  correspondant à la nouvelle limite. Passer de plusieurs fichiers à un seul
  laisse les anciennes rotations indéfiniment.

### Configuration et diagnostic

- **P2 : `trusted_networks` est contredit par le préflight.**
  `internal/config/validate.go:272-273` rejette une IP littérale non privée
  déclarée en zone LAN, même explicitement couverte par un CIDR fiable.
  Reproduit : le transport classe l'adresse synthétique en LAN, mais la
  configuration la refuse. Utiliser une classification commune ; inutile
  d'ajouter une résolution DNS au préflight. Reproduction rejouée sur HEAD :
  le rejet persiste malgré `add0d10`, qui a notamment rendu l'ordre de
  validation déterministe.
- **P2 : `doctor` exige parfois un outil inutilisé.**
  `internal/doctor/doctor.go:180-188` exige `wl-copy` pour `paste`, même avec
  `clipboard_mode="none"`. Or `internal/sink/sink.go:82-83` passe alors
  directement par la frappe. Une configuration fonctionnelle peut donc
  produire un diagnostic rouge.
- **P2 : le nom réservé `mixed` est accepté comme provider réel.**
  `internal/config/validate.go:130-146` l'accepte, alors que
  `internal/cli/composable.go:107-108` l'interdit dans les segments du contrat
  JSON. L'acceptation au préflight est reproduite ; la contradiction de sortie
  est établie par lecture. Réserver ce nom à la validation. Le préflight
  accepte encore ce nom dans la reproduction rejouée sur HEAD.
- **P3 : le health check du cœur est surtout une promesse documentaire.**
  `docs/providers.md:65-68` annonce un appel à `Health` par le cœur et
  `doctor`. Le seul appel de production trouvé est celui de `doctor`, à
  `internal/doctor/doctor.go:307`. Préférer une correction du contrat
  documentaire à l'ajout automatique d'une requête avant chaque transcription,
  sauf besoin fonctionnel explicite.

### Helpers et vérification

- **P2 : des descendants d'un helper OSD peuvent survivre au leader.**
  `internal/sysexec/sysexec.go:219-238` considère le processus terminé dès la
  mort du leader ; un `Kill()` ultérieur ne signale plus son groupe. Cela
  concerne notamment un helper personnalisé qui lance un enfant puis sort.
  La correction doit rester spécifique à la supervision attendue : ne pas
  tuer aveuglément les descendants volontairement persistants de `wl-copy`.
  L'ajout de `RunDiscard` ne corrige pas cette supervision. Les tests de groupe
  gardent le leader vivant jusqu'à l'annulation et ne couvrent pas ce cas.
- **P3 : le helper Python confond `HUP` avec un pipe vidé.**
  `helpers/osd-gtk/murmur-osd.py:169-187` effectue une seule lecture puis ferme
  sur `HUP`. Reproduit avec entrées simulées : un terminal placé après les
  premiers 16 Kio reste non lu. Drainer jusqu'à une véritable lecture EOF.
- **P2 : `make installcheck` peut donner un faux vert.**
  La recette `Makefile:74-106` enchaîne ses commandes avec `;`, sans arrêt sur
  erreur. Une assertion intermédiaire peut échouer, puis le dernier `test`
  réussir et donner un code zéro à l'ensemble. Ajouter `set -eu` au début de
  cette recette est une correction petite et utile. Constat issu de la
  lecture, pas d'un échec d'installation provoqué.

## Constats résolus

### Buffer IPC au handshake

Le correctif `6c646da` conserve le même `json.Decoder` pour l'accusé puis les
événements : `internal/ipc/client.go:63-85`, helper à `:128-132`. Les octets
préchargés ne sont plus perdus entre deux décodeurs.

`TestSubscribePreservesEventBufferedWithHandshake`, à
`internal/ipc/ipc_test.go:289`, écrit l'accusé et l'événement ensemble. Les tests
d'abonnement passent dix répétitions sous `-race`. Le défaut initial est clos ;
la perte d'événements sous surcharge, explicitement documentée, est un autre
contrat et n'est pas remise en cause par cette correction.

### Clear transient après échec de copie

Le correctif `985802d` conserve l'erreur de copie, interdit l'injection après
cet échec, tente le clear et joint les erreurs :
`internal/sink/sink.go:170-188`. La documentation correspondante est mise à jour
dans `docs/sinks.md:38-43`.

La régression de `internal/sink/sink_test.go:523-542` vérifie exactement un
clear, aucune injection et les deux causes d'erreur. Elle passe, comme les
tests du budget compensatoire et de l'annulation explicite.

Ce constat portait sur l'absence de tentative, pas sur une garantie
d'effacement. Une annulation explicite reste attachée au contexte, le clear
peut échouer et les modes persistants ne sont pas effacés.

## Intégration desktop

Cette section conserve les observations locales de la revue sur `69a2e643`.
Leur validité actuelle n'est pas attestée par cette actualisation du code.
Les conséquences du nouveau contrat de collage sont distinguées ci-dessous ;
elles ne prouvent pas que le binaire installé a été mis à jour.

### Points à corriger ou à décider

#### Binding physique de secours pour annuler

Dans `~/dotfiles/hypr/modules/bindings-common.lua:76-82`, le secours
`Super+code:9` est déclaré. Dans la table de l'instance Hyprland active,
l'entrée correspondante expose `key=""`, `keycode=0`, et aucune liaison
`keycode=9` n'apparaît.

Le binding symbolique `Super+escape` existe. Cela ne signifie donc pas que
toute annulation au clavier est cassée, mais le secours prévu n'est pas
opérationnel tel qu'enregistré, malgré `configerrors` vide. La cause exacte
côté API Lua et la frappe physique restent à valider.

#### Retours à la ligne actifs

`~/dotfiles/murmur/config.toml:79-85` active `dangerous-keep` et conserve donc
les retours à la ligne dans l'injection active. Dans un terminal, leur effet
dépend du traitement du collage. Sur HEAD, le collage ne retombe plus sur la
frappe après un échec : la mention initiale d'une « frappe de repli » est donc
obsolète pour ce chemin. Le risque de soumettre des lignes par frappe reste
pertinent pour une frappe directe (`type`, clipboard `none` ou liste de
raccourcis vide).

C'est un choix explicite, pas un bug. Pour un raccourci global susceptible de
viser un shell, préférer `space`, avec `clipboard-only` pour les sorties
multilignes.

#### Limites du raccourci de collage

La liste commence par `ctrl+shift+v`, puis `shift+insert`, puis `ctrl+v` :
`~/dotfiles/murmur/config.toml:82`.

Sur la révision initiale, la cascade s'arrêtait dès que `wtype` réussissait,
sans vérifier la consommation par l'application. Sur HEAD,
`internal/inject/inject.go:64-78` n'émet désormais que le premier raccourci,
même s'il échoue. Aucun raccourci suivant ni frappe de secours n'est alors
tenté. Une liste vide choisit directement la frappe.

Avec la liste historiquement observée, seul `ctrl+shift+v` serait utilisé par
le code actuel. `shift+insert` et `ctrl+v` ne sont plus des secours techniques.
Un succès de `wtype` confirme toujours seulement l'émission des touches, pas
le collage dans l'application.

Le comportement est documenté, mais sa limite est importante pour l'usage
global GTK/Qt/terminal. Les tests `TestPasteUsesOnlyFirstShortcut` et
`TestPasteWithoutShortcutTypesOnce` passent ; aucun échec applicatif n'a été
provoqué pendant cette revue. Les documents normatifs sont alignés, notamment
`docs/desktop-validation.md:131-135` et `docs/sinks.md:98-103`.

#### Historique clipboard

Le desktop utilise `raw-and-clean`, à `~/dotfiles/murmur/config.toml:81`.
Les watchers `wl-paste --watch cliphist store` sont actifs, lancés par
`~/dotfiles/hypr/sessions/quickshell/autostart.lua:29-30`.

Le brut et le résultat final sont donc exposés à `cliphist`, indépendamment de
la rétention audio. `clipboard-only` n'y change rien. Aucun contenu de la base
n'a été lu ; la conservation effective de chaque copie n'est pas garantie par
la seule présence des watchers.

Si conserver les deux versions est voulu, l'intégration est cohérente. Sinon,
`clean-only` évite l'exposition du brut ; seul un chemin sans clipboard évite
cette collecte.

#### Documentation de migration

Le son est activé dans `~/dotfiles/murmur/config.toml:90-91`, alors que
`docs/production-migration.md:71-72` le présente encore comme à valider avant
activation. Les lecteurs et le fichier sonore existent ; audibilité et timing
n'ont pas été testés.

### Éléments vérifiés initialement

| Domaine | État vérifié |
|---|---|
| Dotfiles | `~/.config/murmur` pointe vers `~/dotfiles/murmur` ; TOML propriétaire Mathieu, mode `0600`. Hyprland et zsh utilisent également les liens vers les dotfiles. |
| Installation | Binaire et helper sous `~/.local/bin` ; `murmur help` fonctionne hors checkout. Documentation installée présente. |
| Version | Lors de la revue initiale, binaire installé identique à `build/murmur`, métadonnées correspondant à `69a2e643`, `vcs.modified=false`. Aucune correspondance au nouveau HEAD vérifiée. |
| Hyprland | Version 0.56.2, session active Quickshell, import des bindings communs, raccourcis principaux présents. |
| Exécutables | Les bindings utilisent le chemin absolu de Murmur, évitant une dépendance au PATH pour le lancer. |
| Audio | PipeWire et WirePlumber actifs ; source par défaut RØDE NT-USB+ Mono, non muette. `audio.source=""` suit ce défaut sans verrouiller ce microphone. |
| Notifications | Dunst possède `org.freedesktop.Notifications` ; ses règles correspondent aux catégories émises par Murmur. |
| Cycle de vie | L'absence de service systemd Murmur est normale pour le modèle de session détachée. Aucun processus Murmur ou `pw-record` au moment de l'inspection. |
| Runtime | `/run/user/1000/murmur` appartient à Mathieu et est en `0700`. |

La configuration sur disque concorde aussi avec la migration sur le STT LAN,
le modèle français, le cleanup OpenRouter, le découpage post-capture à 60
secondes, l'absence de fallback STT, le live chunking et le plugin désactivés,
ainsi que les guillemets ASCII. Aucune requête vers les destinations n'a été
effectuée.

## Simplicité et conception

La structure générale est raisonnable, mais la cohérence entre chemins
d'exécution est moins bonne que la qualité locale des composants. Une
réécriture n'est pas recommandée.

Les bons choix sont concrets : commandes sans shell, interfaces testables,
machine d'état explicite, séparation capture/STT/processing/livraison,
contrôle réseau centralisé, secrets exclus des diagnostics. La sécurité autour
des descripteurs et des credentials n'est pas une abstraction décorative.

Les simplifications les plus rentables :

1. **Clarifier la propriété de l'audio.** « Fermer », « libérer », « conserver »
   et « finir la session » sont trop liés implicitement. C'est la première
   simplification à faire, avant toute factorisation.
2. **Partager le traitement batch d'un WAV déjà ouvert.**
   `internal/session/orchestrator.go:463-503` et
   `internal/core/transcribe.go:18-69` répètent découpage, requêtes et
   nettoyage, avec des appels STT différents. Une fonction commune étroite
   réduirait les divergences ; pas besoin d'un framework de workflows.
3. **Partager les primitives sûres des deux logs, pas leurs politiques.**
   Leur duplication a déjà produit des écarts de sécurité et de rotation.
   Une ouverture validée commune suffit ; fusionner les writers et leurs
   files compliquerait inutilement.
4. **Supprimer les abstractions sans usage.** `Text` et `Stage` dans
   `internal/textpipe/pipeline.go:33-43`, ainsi que `LiveRecording.Errors()`
   dans `internal/audio/live.go:35`, ajoutent un modèle parallèle ou un canal
   sans consommateur actuel.
5. **Remplacer les algorithmes artisanaux coûteux : partiellement résolu.**
   Les deux tris STT par insertion ont été remplacés par `sort.Slice` dans
   `95a3b5a` : appels à `internal/stt/stt.go:506` et `:625`, implémentations
   à `:634-639`. Cela résout le coût quadratique signalé ; le choix de
   `sort.Slice` plutôt que `slices.SortFunc` n'appelle pas un nouveau refactoring.
   Les tests d'ordre passent, sans benchmark à 65 536 segments. Restent les
   concaténations Markdown répétées dans
   `internal/plugin/markdown/markdown.go:363-383`. Le contrôle de fuite des tokens
   dans `internal/textpipe/render.go:72-75` parcourt aussi le résultat une fois par
   token ; une recherche unique du namespace généré serait plus économique.
6. **Réduire la duplication documentaire.** Le README de plus de 500 lignes
   répète beaucoup de contrats détaillés ailleurs. Les écarts sur le health
   check et le son illustrent le coût. Garder le README centré sur
   installation, usage et choix de données ; mettre les garanties précises
   dans un document de référence par sujet.

Conserver la machine d'état et les workers UI distincts : leurs différences
correspondent à de vrais comportements, pas seulement à du code similaire.
Le timeout du plugin borne l'attente de l'appelant, pas le travail d'un plugin
ignorant son contexte ; pour le Markdown local, un builder et une coopération
avec le contexte seraient plus utiles qu'une couche d'abstraction
supplémentaire. Cette limite n'est pas une garantie de sandbox cachée : elle
est déjà documentée.

## Vérifications et limites

### Résultats sur HEAD

- `make lint` passe sur `fcd52c7`.
- La suite complète `go test -mod=readonly -race -count=1 ./...` passe dès le
  lancement de cette actualisation, avec `GOPROXY=off`, `GOSUMDB=off` et
  `GOTOOLCHAIN=local`.
- Lors de la revue initiale, le premier lancement avait échoué sur
  `TestOpenAICompatibleClassifiesResponses/large_response` : timeout au lieu
  de `server_error`. Le test impose une seconde pour traiter plus de 4 Mio
  sous `-race`, à `internal/stt/openai_test.go:160-168`. Dix répétitions
  isolées avaient ensuite passé : test vraisemblablement sensible à la charge.
  Le fichier de test est inchangé ; le problème n'a pas été reproduit pendant
  cette actualisation, sans que cela démontre une correction.
- Les deux tests Python OSD passent à nouveau avec
  `python3 -B -m unittest discover -s helpers/osd-gtk -p 'test_*.py'`.
- Les trois défauts prioritaires sont à nouveau reproduits sous `-race` avec
  des tests temporaires injectés par overlay, sans modifier le dépôt. Les processus audio et les
  providers sont simulés ; les adaptateurs et mécanismes de rétention ou de
  budget concernés sont ceux du projet. Les contrôles session avec rétention
  désactivée ou budget suffisant passent dix fois. Les reproductions config
  continuent aussi de montrer le refus d'une IP LAN fiable et l'acceptation de
  `mixed`.
- Les tests ciblés d'abonnement IPC, de concurrence session, de provenance et
  budgets STT et de transcription core passent dix répétitions sous `-race`.
  Les tests versionnés ne couvrent toujours pas les combinaisons des trois P1.
  Aucun `WARNING: DATA RACE` n'est observé ; le constat statique sur
  `runResult` n'est ni reproduit ni invalidé par ce résultat.
- Les tests locaux utilisent Go `1.27.0-X:nodwarf5` ; la matrice CI déclarée
  Go 1.23/1.26 n'a pas été rejouée. Le binaire desktop examiné a été construit
  avec Go `1.26.5-X:nodwarf5` lors de l'inspection initiale.

### Limites desktop

- Aucun microphone, collage, frappe, notification, son ou appel provider réel
  n'a été déclenché. Aucun secret ni contenu du clipboard n'a été lu.
- `doctor` n'a pas été lancé contre la configuration réelle, puisqu'il peut
  résoudre des credentials et effectuer des requêtes. Les constats desktop
  portent sur le TOML et les services observés, pas sur d'éventuelles
  surcharges `MURMUR_*`.
- L'environnement de l'outil pointait initialement vers une ancienne instance
  Hyprland. L'instance active a été retrouvée puis interrogée explicitement.
  Cela ne démontre pas un problème dans l'environnement des raccourcis.
- Capture réelle, livraison GTK/Qt/terminal, rendu Dunst, son, OSD réel,
  reconnexion et cycles prolongés restent non vérifiés. Les validations
  historiques de juillet n'ont pas été reproduites pendant cette revue.

### Reproductions temporaires

Les fichiers suivants ont été laissés sur la machine de revue sous
`/tmp/opencode`. Ils ne font pas partie du dépôt et peuvent disparaître ; les
commandes ne sont donc pas reproductibles depuis un clone seul.

```text
/tmp/opencode/murmur-p1-overlay_test.go
/tmp/opencode/murmur-p1-overlay.json
/tmp/opencode/murmur-review-budget_test.go
/tmp/opencode/murmur-review-overlay.json
```

Reproductions session rejouées sur HEAD, échec attendu puisque les assertions
expriment le comportement correct :

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
go test -overlay=/tmp/opencode/murmur-p1-overlay.json \
  -race -count=1 -timeout=30s -v ./internal/session \
  -run '^TestReviewP1'
```

Contrôles session rejoués sur HEAD, succès attendu :

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
go test -overlay=/tmp/opencode/murmur-p1-overlay.json \
  -race -count=10 -timeout=30s ./internal/session \
  -run '^TestReview(RetentionDisabledControl|LiveWithinBudgetControl)$'
```

Reproductions du budget STT et de la validation des providers rejouées sur HEAD,
échecs attendus :

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
go test -overlay=/tmp/opencode/murmur-review-overlay.json \
  -race ./internal/stt -run '^TestReview' -v -count=1
```

## Ordre recommandé

1. Corriger les trois défauts prioritaires et ajouter leurs tests de
   combinaison au dépôt.
2. Traiter la course sur `runResult`, les guillemets, l'ouverture des fichiers
   et la rotation. Le buffer IPC et le clear transient ne sont plus à corriger.
3. Réparer le binding de secours et décider explicitement de la politique
   d'injection multilignes et d'historique clipboard.
4. Corriger les autres contrats et simplifier les duplications.

Le besoin principal n'est pas davantage de couches de protection, mais des
garanties cohérentes de bout en bout.
