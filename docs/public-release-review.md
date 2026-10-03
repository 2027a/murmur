# Murmur : préparer une solution générale de dictée Linux/Wayland

## Verdict

Le moteur est déjà largement réutilisable. Le principal obstacle n'est pas
la transcription : c'est le contrat de livraison sur les différents bureaux,
puis le parcours permettant à une personne extérieure d'arriver à sa première
dictée sans disposer du homelab et des dotfiles du développeur.

Aujourd'hui, Murmur est un client de dictée PipeWire à providers HTTP, avec une
intégration éprouvée partiellement sur Hyprland. Ce n'est pas encore une
solution de saisie automatique générale sur GNOME, KDE et wlroots.

Il faut conserver ce qui fonctionne, rendre les capacités explicites et
réaliser les intégrations manquantes. Une refonte du moteur, un gestionnaire
de modèles universel ou une collection de nouveaux services ne sont pas des
prérequis justifiés par cette revue.

**Clipboard + notifications est un premier parcours public raisonnable, à
valider par bureau. Ce repli ne suffit pas à revendiquer une dictée automatique
universelle.** Une release limitée à des bureaux nommés est possible avant
l'atteinte de cette ambition générale, à condition de l'annoncer clairement.

## Périmètre et preuves

Revue du 3 octobre 2026 sur `release`, créée depuis `main` :

```text
6052defc07a8d85a26a62014a72d23c4855a4786
```

Les références de lignes concernent ce commit, pas un futur correctif.
La revue distingue les défauts établis par lecture, les essais exécutés et les
compatibilités restant à vérifier sur un vrai bureau.

- Lecture du code, des tests, des exemples, du Makefile et des docs actives.
- Consultation des sources de `wtype` v0.4 et des protocoles concernés.
- Vérification du dépôt distant, des tickets, du tag et des releases.
- Compilation native et cross-compilation Linux arm64.
- Validation locale Go et Python, sans provider personnel ni bureau graphique.
- Aucun correctif fonctionnel, déploiement ou changement de raccourci réalisé.

Les tickets issus de cette revue constituent une file d'attente, pas des
correctifs déjà en cours. Héphaïstos porte les propositions techniques ;
Mathieu arbitre les choix de produit et autorise les fusions.

## 1. Ce qui est déjà un bon socle

### Architecture

Le chemin principal est clair :

```text
CLI / raccourci externe
  → processus de session et IPC Unix
  → capture PipeWire / WAV canonique
  → STT HTTP, segmentation et fallback
  → intentions protégées, cleanup optionnel, rendu, plugin optionnel
  → sink et interface de statut
```

Les commandes `transcribe`, `cleanup` et `dictate` permettent de réutiliser le
moteur sans l'intégration desktop. Le chunking live anticipe le travail STT ;
il ne constitue pas une insertion incrémentale du texte dans l'application.
La livraison reste finale et unique.

Le découplage entre moteur, recorder, exécuteur, UI et sink facilite les tests
et les extensions ciblées. Il n'est pas nécessaire de remplacer cet ensemble
pour généraliser le produit.

### Sécurité et exploitation

Les mécanismes présents méritent d'être conservés :

- policy réseau local/LAN/internet, contrôle des adresses et refus des
  redirections ; proxies d'environnement désactivés ;
- sources de secrets explicites, sans clé en clair dans le TOML ;
- fichiers privés, validation des chemins et snapshots WAV scellés ;
- annulation, verrou de session et livraison au plus une fois ;
- événements et dernier résultat sans transcript ;
- sanitisation des caractères de contrôle avant injection active ;
- cleanup et plugin optionnels, avec repli lorsque leur résultat est invalide ;
- installation `PREFIX`/`DESTDIR`, désinstallation et tests d'installation.

Ces garanties ne prouvent ni l'absence de modification sémantique par un LLM,
ni l'effacement de l'historique d'un gestionnaire de clipboard, ni l'absence
de rétention par un provider distant. Le plugin Go interne n'est pas un
sandbox pour du code tiers.

## 2. Le principal chantier : la livraison Wayland

### Capacité, pas nom du système

`internal/inject/inject.go:56-75` utilise exclusivement `wtype`. Le mode
Le mode `auto` choisit `paste` ou `type`, sans détecter
un backend compatible avec le bureau.

`wtype` v0.4 utilise `zwp_virtual_keyboard_manager_v1`. Le fait de tourner sous
Wayland ou d'avoir installé cet exécutable ne garantit pas ce protocole.
Les chemins GNOME/Mutter et KDE/KWin standards examinés ne fournissent pas
l'intégration attendue par ce backend. Leur support ne peut pas être déduit
de l'intégration Hyprland.

Suivi : [#69, compatibilité de livraison][issue-69].

### Matrice attendue, non validée pendant cette revue

- **Hyprland, Sway et autres compositeurs compatibles** : capture PipeWire,
  clipboard et injection attendus lorsque les protocoles sont disponibles.
  Seul le périmètre Hyprland décrit dans les documents existants bénéficie
  d'essais desktop suivis ; cela ne valide pas tous les compositeurs wlroots.
- **GNOME/Mutter** : capture et clipboard à tester selon les versions ; pas de
  support revendicable de l'injection actuelle. L'OSD fourni repose sur
  layer-shell, non supporté par GNOME selon sa documentation.
- **KDE/KWin** : capture et clipboard à tester, notamment avec Klipper ; pas de
  support revendicable de l'injection actuelle. Layer-shell est supporté selon
  la bibliothèque, mais cela ne valide pas le rendu réel de l'OSD Murmur.

Le fallback de `wl-copy` sans protocole data-control peut créer une surface
pour obtenir le clipboard, avec des conséquences de focus ou de blocage.
Même le parcours clipboard doit donc être testé, et non déclaré universel
par simple présence de `wl-copy` dans `PATH`.

### Chemin recommandé

1. Garder `wtype` comme backend des compositeurs réellement compatibles.
2. Publier les capacités vérifiées, bureau et version compris.
3. Prototyper un backend autorisé GNOME/KDE via le portail RemoteDesktop.
   L'interface actuelle propose `ConnectToEIS` pour un contexte libei.
4. Valider consentement, restauration et fermeture de session, disposition
   clavier, Unicode, touches modificatrices et comportement du focus.
5. Proposer clipboard + notifications comme repli explicite et non invasif.

Le portail est une piste fondée sur une API existante, pas une implémentation
Murmur ni une garantie de compatibilité déjà testée. Sa faisabilité doit être
exercée avant de choisir l'abstraction définitive.

Ne pas résoudre ce problème par un accès implicite à `uinput`, un daemon root
ou une permission générale sur les périphériques d'entrée.

### Focus et raccourci de collage

Le texte est envoyé au focus présent lors de la livraison, pas à une fenêtre
identifiée au démarrage. Une autre application peut donc recevoir une dictée
si le focus change pendant le STT ; une frappe longue peut aussi être répartie
entre des fenêtres. Ce contrat doit être explicite.

Le défaut propose plusieurs raccourcis, mais seul le premier est émis :
`ctrl+shift+v`. Un code de sortie positif signifie que les touches ont été
émises, pas que l'application a consommé le texte.

Ne pas réintroduire une cascade automatique de collages : les tentatives
ambiguës peuvent produire des actions ou insertions multiples. Choisir un
chemin adapté à la cible et fournir une récupération manuelle explicite.

## 3. Défauts de livraison à corriger

### Haute priorité : `shift+insert` n'est pas une touche Insert

`internal/inject/inject.go:85-92` passe l'argument nu `Insert`. Le parseur
amont traite cette forme comme du texte ; une touche nommée exige sa syntaxe
spécifique. Son test valide actuellement les mauvais arguments :
`internal/inject/inject_test.go:50-57`.

Corriger les arguments et tester un collage réel, sans frappe du mot Insert.

Suivi : [#70, touche Insert][issue-70].

### Haute priorité : cinq secondes ne suffisent pas à une frappe longue

`internal/inject/inject.go:49-70` applique le budget fixe de
`internal/delivery/budget.go:5-7`. Dans `wtype` v0.4, chaque caractère comporte
au minimum deux attentes de 2 ms, hors aller-retour Wayland. Un texte de
2 000 caractères demande donc au moins 8 secondes d'attente.

Les modes `type`, `paste+none` et collage sans raccourci peuvent échouer après
une insertion partielle. L'estimation vient du code amont, pas d'un essai de
frappe réel pendant cette revue.

Distinguer le budget d'un raccourci et celui d'un texte, conserver une borne
et une annulation, et ne jamais rejouer aveuglément une insertion partielle.

Suivi : [#71, budget de frappe][issue-71].

### Priorité moyenne : effacement transient d'un clipboard étranger

`internal/sink/sink.go:170-185` appelle le clear même si la copie a échoué.
Une copie extérieure survenue pendant la livraison peut également être effacée.

Le ticket #24 avait volontairement ajouté le nettoyage après échec ; il ne
faut pas annuler ce travail sans traiter le nouveau compromis de propriété.
Définir ce que le backend peut garantir, puis sécuriser ou limiter le mode.
`clean-only`, défaut actuel, n'est pas bloqué par cette question.

Suivi : [#75, propriété et clear transient][issue-75].

## 4. Fiabilité du moteur

### Haute priorité : bootstrap non contrôlable pendant les credentials

`internal/cli/start_unix.go:258-279` construit la session avant d'ouvrir l'IPC.
La construction résout synchroniquement les credentials STT et cleanup.
Un helper lent empêche donc `status`, `stop` et `cancel` de joindre la phase
de préparation, contrairement au contrat annoncé dans `docs/cli.md:30-35`.

Le timeout parent tue le processus principal ; le nettoyage des groupes
séparés de helpers doit être testé. Leur survie n'a pas été reproduite ici.

Exposer le contrôle avant les opérations lentes et garantir l'arrêt borné
du bootstrap comme de ses descendants.

Suivi : [#72, bootstrap et helpers][issue-72].

### Haute priorité : cleanup explicitement tronqué accepté

`internal/cleanup/cleanup.go:156-167` ignore `finish_reason`. Une réponse
non vide marquée `length` peut remplacer une dictée entière par un fragment,
sans warning, lorsqu'elle ne contient pas de sentinelles protégées.
`internal/textpipe/pipeline.go:123-128` accepte alors ce résultat.

Traiter les terminaisons explicitement incomplètes comme une dégradation et
conserver le texte précédent. Ne pas imposer un champ obligatoire aux serveurs
compatibles qui l'omettent. Ce constat repose sur la lecture déterministe ;
la probe HTTP supplémentaire n'a pas été exécutée.

Suivi : [#73, troncature cleanup][issue-73].

### Priorité moyenne : capture brute et ressources audio longues

La contre-pression STT bloque la production de segments, mais pas le processus
`pw-record`, qui continue son WAV. Les limites texte et segments ne donnent
pas un plafond indépendant de durée ou d'octets enregistrés.
Le PCM mono 16 kHz, s16 représente environ 110 Mio par heure.

Par ailleurs, chaque segment possède un snapshot memfd scellé conservé jusqu'à
la fermeture ; le batch matérialise tous les segments avant transcription.
La consommation de mémoire et de descripteurs ne dépend donc pas seulement
du nombre de workers. Le fallback impose de garder un audio rejouable.

Définir un budget de capture indépendant et une conservation rejouable bornée.
Ne pas simplement fermer les segments après STT en cassant le fallback.

Preuves : `internal/audio/pwrecord/live.go:200-206`,
`internal/audio/live.go:169-195`, `internal/core/transcribe.go:52-64`,
`internal/audio/wav/snapshot_linux.go:13-40`.

Suivi : [#76, capture brute][issue-76] et
[#77, snapshots et descripteurs][issue-77].

### Basse priorité : faux rejets cleanup

Le filtre de `internal/cleanup/cleanup.go:186-195` utilise des préfixes sans
frontière ni comparaison avec l'entrée. « Voici le compte rendu. » ou
« Surely this is correct. » peuvent être rejetés même s'ils ont été dictés.
Le repli conserve le texte : il s'agit de correction inutilement dégradée,
pas de perte directe de la dictée.

Suivi : [#84, préfixes méta][issue-84].

## 5. Une dictée générale doit pouvoir être littérale

Désactiver le plugin ne désactive pas les intentions vocales :
`internal/textpipe/pipeline.go:102,132` appelle toujours protection et rendu.
Une expression ordinaire telle que « à la ligne » peut donc devenir une
commande, sans mode littéral ou échappement utilisateur.

Les défauts de transcription et de prompt cleanup sont français. Les packs
FR/EN existent, mais le Markdown reste français. Les autres langues STT sont
configurables sous certaines combinaisons de pack ; cela ne signifie pas que
les commandes, la typographie et l'interface sont multilingues.

Prévoir un mode littéral simple, séparer ces notions de langue et publier les
limites exactes. L'auto-détection de langue ne doit pas être promise sur la
base du seul nom `command_pack=auto`.

Suivi : [#78, littéral et langues][issue-78].

Le silence mérite également un corpus d'essai. La présence de données et la
durée utile du WAV ne prouvent pas la présence de parole. Une hallucination
STT sur silence n'a pas été reproduite pendant cette revue ; ne pas ajouter
un système VAD complexe avant d'en mesurer le besoin.

## 6. Première dictée, diagnostic et interface

### Parcours autonome

Le défaut suppose `http://127.0.0.1:8000/v1`, `large-v3` et le français.
Murmur ne fournit ni serveur STT ni installation du modèle. La référence est
riche, mais ne constitue pas un parcours autonome de première utilisation.

Le guide public doit faire réussir ce chemin :

```text
installer Murmur
  → choisir un serveur local documenté ou un cloud explicitement autorisé
  → configurer langue, endpoint, modèle et source de secret
  → vérifier les capacités avec doctor
  → réussir une dictée courte sans injection
  → sélectionner la livraison adaptée au bureau
  → poser et tester les raccourcis
```

Fournir des exemples complets local et cloud, un guide court et la référence
technique séparée. Les profils de confidentialité doivent expliquer le trajet
audio/texte, les copies clipboard et les rétentions effectivement garanties.
Il n'est pas nécessaire de gérer tous les serveurs de modèles dans Murmur.

Des recettes GNOME, KDE, Sway et Hyprland sont le minimum utile pour les
raccourcis. Un portail GlobalShortcuts ou une interface de configuration peut
venir après observation des difficultés ; aucun daemon permanent n'est requis
par le produit actuel simplement pour exécuter ces commandes.

Le parcours documenté aujourd'hui est principalement push-to-toggle. Une
promesse push-to-talk doit distinguer maintien de touche et appui bascule.

Suivi : [#79, première dictée][issue-79].

### `doctor` : présence n'est pas capacité

Les checks `wtype` et `wl-copy` vérifient essentiellement `PATH`. Le check OSD
réussit après les imports, sans vérifier display et layer-shell.
`paste+clipboard_mode="none"` peut réclamer `wl-copy` alors que le sink ne
l'utilise pas.

Distinguer configuration valide, outils présents, protocoles accessibles,
provider vérifié et essai de bout en bout. Un health check désactivé reste
un check non exécuté, pas une preuve de disponibilité du serveur.

Le diagnostic ne doit pas injecter du texte dans une application à l'insu de
l'utilisateur. Un test de livraison doit être une action explicite.

Suivi : [#74, capacités doctor][issue-74].

### Statut et récupération

Une personne utilisant un raccourci ne devrait pas devoir deviner quel log
consulter après « Échec de la dictée ». Afficher une erreur contrôlée utile,
sa classe et une action suivante, sans publier stderr brut, clé ou transcript.

La récupération du texte après échec dépend actuellement d'une copie
persistante éventuellement réussie ; le dernier résultat ne conserve pas
le transcript. Documenter cette limite. Si une récupération supplémentaire
est proposée, elle doit être explicite, privée et bornée, pas un journal
secret de toutes les dictées.

### QuickShell et OSD

Le défaut générique `notify` est indépendant des dotfiles. Le frontend
QuickShell décrit dans `docs/quickshell.md` est personnel et hors dépôt :
le protocole est réutilisable, pas le module installable depuis Murmur.

Ne pas rendre ce frontend nécessaire à la release. Un exemple autonome peut
être fourni si son usage est retenu. Tester aussi notifications masquées,
plein écran et absence de feedback ; l'OSD layer-shell n'est pas un frontend
universel pour GNOME.

## 7. Livraison publique et documentation

Le Makefile et la licence MIT donnent une base correcte. Un tag
`v0.1.0-rc1` existe, mais aucune release Forgejo avec artefacts n'est publiée.
`murmur --version` est actuellement une commande inconnue.

Pour une release réellement utilisable :

- artefacts Linux amd64 et arm64 versionnés, sommes de contrôle et licence ;
- version et commit identifiables par le programme ;
- instructions sans compilation obligatoire pour le parcours binaire ;
- dépendances système conditionnelles, chemins XDG et désinstallation clairs ;
- guide court, matrice de support, dépannage et limites de confidentialité ;
- historique des migrations personnelles séparé des guides publics ;
- QuickShell clairement optionnel, sans dépendance cachée aux dotfiles.

Un premier paquet de distribution peut suivre le besoin réel. AppImage,
Flatpak, moteur embarqué et catalogue de plugins ne sont pas tous des
prérequis : ils introduisent des contraintes à justifier séparément.

Suivi : [#80, kit de release][issue-80].

## 8. Validation exécutée et restant à faire

### Résultats de cette revue

Sur cette VM sans session graphique, avec Go 1.24.4 :

- `go mod verify`, `go mod tidy -diff`, format Go et `go vet` réussis ;
- build natif réussi ;
- cross-build Linux arm64 réussi, métadonnées Go vérifiées, non exécuté ;
- tests Python OSD et installcheck réussis ;
- `make installcheck` réussi avec le vrai binaire installé dans un staging ;
- suite Go complète avec détecteur de courses réussie sous umask privée et
  exécution séquentielle des packages ;
- suite parallèle non entièrement verte : course intermittente de reprise
  après livraison bloquée, décrite ci-dessous.

Aucun test réel de microphone, injection, compositor ou provider cloud n'a
été exécuté dans cette VM. Les serveurs contrôlés des tests ne sont pas des
mesures de qualité d'un provider réel.

### Environnement et reproductibilité des tests

Le TMPDIR de la VM possède des ancêtres modifiables par le groupe et des
chemins dépassant la capacité de certains sockets Unix de test. Un namespace
utilisateur/montage a exposé le même scratch à `/mnt`, sans changement de
permissions ni création de temporaires dans le `/tmp` système.

Les fixtures session de rétention échouent sous umask `0002` et réussissent
sous `0077` : la base de `T.TempDir` peut être en `0775`, refusée légitimement
par le moteur. Les fixtures doivent devenir privées explicitement.

Suivi : [#82, fixtures indépendantes de l'umask][issue-82].

Commande ayant réussi pour toute la suite, tests Python compris :

```sh
unshare -Ur -m sh -c '
  set -eu
  mount --bind "$TMPDIR" /mnt
  export TMPDIR=/mnt
  umask 0077
  make test GO_TEST_FLAGS="-race -count=1 -p=1"
'
```

Le test `blocked_delivery_releases_lock_and_socket` échoue parfois lors du
second démarrage en suite parallèle avec « une session est déjà active ».
Il passe trois fois isolé. La persistance du résultat précède la fermeture
complète de la session et de son verrou ; lire ce résultat n'est pas une
barrière suffisante pour supposer une nouvelle session immédiatement prête.

Le succès avec `-p=1` n'est pas une correction. Synchroniser le test sur la
terminaison réelle et vérifier séparément un éventuel effet utilisateur.
Aucune fuite permanente du verrou n'a été établie.

Suivi : [#83, reprise et synchronisation du test][issue-83].

### CI : ne pas revendiquer un runner inexistant

La PR [#68](https://git.2027a.net/2027a/murmur/pulls/68) retire le workflow
inutilisé, à la demande de Mathieu : aucun runner Forgejo n'est configuré.
Le workflow présent dans le commit audité ne constitue donc pas une CI
opérationnelle. Ne pas réintroduire de CI distante sans accord explicite.
Les commandes locales restent les preuves de validation disponibles.

### Critères de sortie pour le support desktop

Pour chaque bureau et version annoncés, avec le binaire réellement installé :

- texte reçu dans GTK, Qt, navigateur/Electron, terminal et cible XWayland ;
- touches physiques, appui bascule ou maintien, répétition et modificatrices ;
- focus changé pendant traitement et frappe ;
- texte court et long, accents, emoji et retours à la ligne ;
- micro par défaut/explicite, muet ou débranché, erreur audio et annulation ;
- clipboard manager présent/absent et copie concurrente ;
- panne provider, timeout, cleanup dégradé et récupération de texte ;
- notification masquée, plein écran, multi-écran et frontend absent ;
- fermeture et reconnexion graphique ;
- vingt cycles sans processus, verrou, socket ou fichier orphelin.

Compléter avec un corpus non sensible : plusieurs langues revendiquées,
ponctuation, silence, noms propres et dictées longues. Mesurer exactitude,
latence après stop et ressources, pour les providers et matériels nommés.
Ne pas exporter les mesures personnelles comme performances universelles.

Suivi : [#81, validation réelle][issue-81].

## 9. Ordre de travail recommandé

### Étape A : décider et prouver le contrat de bureau

Traiter #69 et #74, en parallèle des petits défauts #70 et #73. Un prototype
autorisé GNOME/KDE doit confirmer la voie de livraison avant de construire
une abstraction de backends plus large. Publier les limites immédiatement.

### Étape B : rendre la dictée fiable et générale

Traiter #71, #72 et #78. Définir les budgets audio de #76 et #77, ainsi que
le compromis transient de #75. Corriger #82 et #83 pour des validations
reproductibles. #84 reste une amélioration cleanup non bloquante pour le STT.

### Étape C : rendre le produit installable et vérifiable

Réaliser #79 et #80, puis exécuter #81 sur les bureaux effectivement retenus.
Un succès automatisé ne ferme pas la validation desktop. Aucune promesse de
support général ne doit précéder les preuves correspondantes.

**Conclusion : la release demande surtout de l'intégration et un contrat
public clair, pas une reconstruction du moteur.** Si l'objectif inclut la
saisie automatique sur GNOME et KDE, le backend autorisé et ses essais sont
un vrai chantier ; la documentation seule ne peut pas les remplacer.

## Sources de dépendances consultées

Les sources ci-dessous ont été lues, pas exécutées. `wtype` n'est pas présent
sur cette VM ; v0.4 est la version examinée, pas une version d'exécution
Murmur épinglée. Les documents `master`/`main` décrivent l'amont consulté et ne
prouvent pas les capacités d'une installation desktop particulière.

- Parseur, virtual-keyboard et temporisation de `wtype` v0.4 :
  <https://github.com/atx/wtype/blob/v0.4/main.c>
- Desktops supportés par gtk-layer-shell :
  <https://github.com/wmww/gtk-layer-shell#supported-desktops>
- Contrat et fallback du clipboard :
  <https://github.com/bugaevc/wl-clipboard/blob/master/data/wl-clipboard.1>
- Interfaces du portail RemoteDesktop, consentement et EIS :
  <https://github.com/flatpak/xdg-desktop-portal/blob/main/data/org.freedesktop.portal.RemoteDesktop.xml>
- Liste des sources Wayland de KWin examinée :
  <https://github.com/KDE/kwin/blob/master/src/wayland/CMakeLists.txt>

[issue-69]: https://git.2027a.net/2027a/murmur/issues/69
[issue-70]: https://git.2027a.net/2027a/murmur/issues/70
[issue-71]: https://git.2027a.net/2027a/murmur/issues/71
[issue-72]: https://git.2027a.net/2027a/murmur/issues/72
[issue-73]: https://git.2027a.net/2027a/murmur/issues/73
[issue-74]: https://git.2027a.net/2027a/murmur/issues/74
[issue-75]: https://git.2027a.net/2027a/murmur/issues/75
[issue-76]: https://git.2027a.net/2027a/murmur/issues/76
[issue-77]: https://git.2027a.net/2027a/murmur/issues/77
[issue-78]: https://git.2027a.net/2027a/murmur/issues/78
[issue-79]: https://git.2027a.net/2027a/murmur/issues/79
[issue-80]: https://git.2027a.net/2027a/murmur/issues/80
[issue-81]: https://git.2027a.net/2027a/murmur/issues/81
[issue-82]: https://git.2027a.net/2027a/murmur/issues/82
[issue-83]: https://git.2027a.net/2027a/murmur/issues/83
[issue-84]: https://git.2027a.net/2027a/murmur/issues/84
