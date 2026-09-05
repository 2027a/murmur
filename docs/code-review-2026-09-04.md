# Revue du code et de l'intégration desktop

Date : 4 septembre 2026. Révision examinée : `69a2e643` (HEAD avant cette
documentation). Périmètre : code Go, helper Python, tests, installation,
documentation et intégration avec le desktop et les dotfiles de Mathieu.

La revue a été menée sans modifier le code ni les dotfiles. Les références de
lignes correspondent à la révision examinée. Les constats desktop sont un
instantané local, pas des propriétés garanties par le dépôt.

## Défauts prioritaires

Trois défauts reproduits remettent en cause des garanties explicites du projet.
Ils concernent des combinaisons d'options, pas nécessairement l'usage actuel.

### 1. P1 : une session annulée peut conserver son audio

Références : `internal/session/orchestrator.go:376-390`,
`internal/session/orchestrator.go:486-488`,
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
les quatre cas, sur dix répétitions sous `-race`.

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
fois, sur dix répétitions.

Conséquences : pas de résultat final, session toujours occupée ; un `cancel`
supplémentaire débloque la situation mais remplace le résultat attendu par
`cancelled`. Le microphone est bien arrêté. La variante `max_text_bytes` est
reproduite ; la variante `max_segments` n'a pas été exécutée séparément.

Correction minimale : surveiller simultanément l'arrêt de capture et la
terminaison STT pendant toute la finalisation ; annuler les producteurs dès que
le consommateur termine prématurément, en préservant l'erreur de limite.

Dans le TOML desktop examiné, `live_chunking` est désactivé.

### 3. P1 : un dépassement STT peut devenir un succès partiel

Références : `internal/stt/stt.go:576-593`, `internal/stt/stt.go:262-290`.

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
une livraison réelle.

Correction minimale : faire primer immédiatement les erreurs de limite et
d'annulation sur la première erreur récupérable.

## Autres défauts

Sauf indication contraire, ces constats sont établis par lecture des chemins
d'exécution, sans reproduction de leur effet sur le desktop.

### Concurrence et texte

- **P2 : perte ou corruption d'événements IPC au handshake.**
  `internal/ipc/client.go:63-82` utilise un décodeur pour l'accusé, puis un autre
  pour les événements. Le premier peut précharger des octets du premier
  événement ; ils sont abandonnés. Réutiliser le même `json.Decoder` suffit.
  Le test existant publie seulement après le retour de `Subscribe`, donc évite
  le cas.
- **P2 : écriture non protégée de `runResult`.**
  `internal/session/orchestrator.go:166` affecte la structure hors de
  `metadataMu`, alors que `currentProvider()` la lit sous ce mutex à
  `:603-606`. Un handler `cancel` déjà engagé peut croiser cette affectation.
  Course identifiée statiquement, non déclenchée sous `-race`. Calculer le
  résultat localement, puis l'affecter sous verrou, sans conserver le verrou
  pendant l'appel à `finalResult`.
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
  `internal/config/validate.go:278-279` rejette une IP littérale non privée
  déclarée en zone LAN, même explicitement couverte par un CIDR fiable.
  Reproduit : le transport classe l'adresse synthétique en LAN, mais la
  configuration la refuse. Utiliser une classification commune ; inutile
  d'ajouter une résolution DNS au préflight.
- **P2 : `doctor` exige parfois un outil inutilisé.**
  `internal/doctor/doctor.go:180-188` exige `wl-copy` pour `paste`, même avec
  `clipboard_mode="none"`. Or `internal/sink/sink.go:82-83` passe alors
  directement par la frappe. Une configuration fonctionnelle peut donc
  produire un diagnostic rouge.
- **P2 : le nom réservé `mixed` est accepté comme provider réel.**
  `internal/config/validate.go:130-165` l'accepte, alors que
  `internal/cli/composable.go:107-108` l'interdit dans les segments du contrat
  JSON. L'acceptation au préflight est reproduite ; la contradiction de sortie
  est établie par lecture. Réserver ce nom à la validation.
- **P3 : le health check du cœur est surtout une promesse documentaire.**
  `docs/providers.md:65-68` annonce un appel à `Health` par le cœur et
  `doctor`. Le seul appel de production trouvé est celui de `doctor`, à
  `internal/doctor/doctor.go:307`. Préférer une correction du contrat
  documentaire à l'ajout automatique d'une requête avant chaque transcription,
  sauf besoin fonctionnel explicite.

### Helpers et vérification

- **P2 : le clear transient est sauté si la copie échoue.**
  `internal/sink/sink.go:166-167` retourne avant la compensation. Une commande
  externe peut avoir produit son effet avant d'échouer ; l'échec ne prouve pas
  que le clipboard est resté intact. Faire passer ce chemin par la tentative
  de nettoyage, sans perdre l'erreur initiale ni la distinction avec une
  annulation explicite.
- **P2 : des descendants d'un helper OSD peuvent survivre au leader.**
  `internal/sysexec/sysexec.go:196-215` considère le processus terminé dès la
  mort du leader ; un `Kill()` ultérieur ne signale plus son groupe. Cela
  concerne notamment un helper personnalisé qui lance un enfant puis sort.
  La correction doit rester spécifique à la supervision attendue : ne pas
  tuer aveuglément les descendants volontairement persistants de `wl-copy`.
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

## Intégration desktop

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
dépend du traitement du collage ; en frappe de repli, ils peuvent soumettre
des lignes.

C'est un choix explicite, pas un bug. Pour un raccourci global susceptible de
viser un shell, préférer `space`, avec `clipboard-only` pour les sorties
multilignes.

#### Limites de la cascade de collage

La liste commence par `ctrl+shift+v`, puis `shift+insert`, puis `ctrl+v` :
`~/dotfiles/murmur/config.toml:82`.

`internal/inject/inject.go:64-77` s'arrête dès que `wtype` réussit. Cela
confirme l'émission des touches, pas le collage. Une application qui ignore
le premier raccourci ne provoquera pas l'essai du suivant.

Le comportement est documenté, mais sa limite est importante pour l'usage
global GTK/Qt/terminal. Aucun échec applicatif n'a été provoqué pendant cette
revue.

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

### Éléments cohérents et vérifiés

| Domaine | État vérifié |
|---|---|
| Dotfiles | `~/.config/murmur` pointe vers `~/dotfiles/murmur` ; TOML propriétaire Mathieu, mode `0600`. Hyprland et zsh utilisent également les liens vers les dotfiles. |
| Installation | Binaire et helper sous `~/.local/bin` ; `murmur help` fonctionne hors checkout. Documentation installée présente. |
| Version | Binaire installé identique à `build/murmur`, métadonnées correspondant au HEAD examiné, `vcs.modified=false`. |
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
   `internal/session/orchestrator.go:459-499` et
   `internal/core/transcribe.go:18-69` répètent découpage, requêtes et
   nettoyage, avec des appels STT différents. Une fonction commune étroite
   réduirait les divergences ; pas besoin d'un framework de workflows.
3. **Partager les primitives sûres des deux logs, pas leurs politiques.**
   Leur duplication a déjà produit des écarts de sécurité et de rotation.
   Une ouverture validée commune suffit ; fusionner les writers et leurs
   files compliquerait inutilement.
4. **Supprimer les abstractions sans usage.** `Text` et `Stage` dans
   `internal/textpipe/pipeline.go:32-42`, ainsi que `LiveRecording.Errors()`
   dans `internal/audio/live.go:35`, ajoutent un modèle parallèle ou un canal
   sans consommateur actuel.
5. **Remplacer les algorithmes artisanaux coûteux.** Les tris par insertion de
   `internal/stt/stt.go:504-516` et `:618-625` devraient être des
   `slices.SortFunc`. Avec 65 536 segments autorisés, c'est à la fois plus
   simple et plus prévisible. Même logique pour les concaténations Markdown
   répétées. Le contrôle de fuite des tokens dans
   `internal/textpipe/render.go:72-75` parcourt aussi le résultat une fois par
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

- `make lint` passe.
- La suite complète `go test -mod=readonly -race -count=1 ./...` passe au
  second lancement, avec `GOPROXY=off`, `GOSUMDB=off` et `GOTOOLCHAIN=local`.
- Le premier lancement échoue sur
  `TestOpenAICompatibleClassifiesResponses/large_response` : timeout au lieu
  de `server_error`. Le test impose une seconde pour traiter plus de 4 Mio
  sous `-race`, à `internal/stt/openai_test.go:160-168`. Dix répétitions
  isolées passent ensuite : test vraisemblablement sensible à la charge.
- Les tests Python OSD passent avec
  `python3 -B -m unittest discover -s helpers/osd-gtk -p 'test_*.py'`.
- Les trois défauts prioritaires sont reproduits avec des tests temporaires
  injectés par overlay, sans modifier le dépôt. Les processus audio et les
  providers sont simulés ; les adaptateurs et mécanismes de rétention ou de
  budget concernés sont ceux du projet.
- Les tests locaux utilisent Go `1.27.0-X:nodwarf5` ; la matrice CI déclarée
  Go 1.23/1.26 n'a pas été rejouée. Le binaire desktop examiné a été construit
  avec Go `1.26.5-X:nodwarf5`.
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

Reproductions session, échec attendu puisque les assertions expriment le
comportement correct :

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
go test -overlay=/tmp/opencode/murmur-p1-overlay.json \
  -race -count=1 -timeout=30s -v ./internal/session \
  -run '^TestReviewP1'
```

Contrôles session, succès attendu :

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
go test -overlay=/tmp/opencode/murmur-p1-overlay.json \
  -race -count=10 -timeout=30s ./internal/session \
  -run '^TestReview(RetentionDisabledControl|LiveWithinBudgetControl)$'
```

Reproductions du budget STT et de la validation des providers, échecs attendus :

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
go test -overlay=/tmp/opencode/murmur-review-overlay.json \
  ./internal/stt -run '^TestReview' -v -count=1
```

## Ordre recommandé

1. Corriger les trois défauts prioritaires et ajouter leurs tests de
   combinaison au dépôt.
2. Traiter IPC, guillemets, ouverture des fichiers et rotation.
3. Réparer le binding de secours et décider explicitement de la politique
   d'injection multilignes et d'historique clipboard.
4. Corriger les autres contrats et simplifier les duplications.

Le besoin principal n'est pas davantage de couches de protection, mais des
garanties cohérentes de bout en bout.
