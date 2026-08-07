# Validation desktop T8.4

Cette checklist sépare les garanties automatisables du comportement observable
sur un vrai bureau Wayland. Elle doit être exécutée avec le binaire installé,
pas depuis `go run` ni depuis un chemin du checkout.

## Préparation

- [ ] Noter distribution, version de Go utilisée au build, compositor et version
  de PipeWire.
- [ ] Installer Murmur avec le `PREFIX` retenu et vérifier que `command -v
  murmur` pointe vers ce préfixe.
- [ ] Si l'OSD est testé, installer avec `INSTALL_OSD=1` et vérifier que
  `command -v murmur-osd` pointe vers le même préfixe.
- [ ] Créer un compte-rendu sans secret, transcript sensible ni copie de fichier
  de credential.
- [ ] Préparer un provider local contrôlé et, pour les sections concernées, un
  provider LAN et un provider cloud de test.
- [ ] Installer uniquement les dépendances conditionnelles nécessaires aux
  variantes testées.
- [ ] Sauvegarder la configuration utilisateur existante avant de remplacer
  `config.toml` pour la validation.

## Installation et autonomie

- [ ] `murmur help` fonctionne depuis un répertoire sans lien avec le checkout.
- [ ] Le binaire trouve ses packs français et anglais sans fichier de données
  externe.
- [ ] L'exemple minimal installé peut être copié dans le répertoire XDG puis
  validé par `murmur doctor`.
- [ ] L'exemple complet installé est accepté après adaptation des endpoints et
  secrets réels.
- [ ] Une installation avec un `PREFIX` non standard fonctionne lorsque son
  `bin` est dans `PATH`.
- [ ] Le helper OSD installé est autonome et `murmur-osd --check` réussit.
- [ ] Une désinstallation avec les mêmes chemins retire binaire, helper, docs et
  exemples, sans retirer configuration, secrets ni état utilisateur.

## Configuration et secrets

- [ ] Une clé TOML inconnue est refusée avant capture.
- [ ] Les surcharges `MURMUR_*` documentées prennent effet et une variable non
  whitelistée est ignorée.
- [ ] `api_key_env` fonctionne sans apparition du secret dans `doctor --json`,
  stderr, événements ou `last-result.json`.
- [ ] `api_key_file` fonctionne avec propriétaire courant et mode `0600`.
- [ ] Un fichier de credential symlinké, appartenant à un autre UID ou lisible
  par groupe/autres est refusé.
- [ ] `api_key_command` fonctionne sans shell et une commande lente est arrêtée
  lors d'une annulation.
- [ ] La configuration refuse plusieurs sources `api_key_*` pour le même
  provider.

## Doctor

- [ ] `murmur doctor` retourne 0 avec la configuration desktop nominale.
- [ ] `murmur doctor --json` produit un rapport v1 lisible par `jq` et cohérent
  avec la sortie humaine.
- [ ] Le runtime créé est un répertoire `0700` accessible en écriture.
- [ ] `pw-record` est détecté et une source explicite valide est vérifiée par
  `pw-cli info` sans lancer de capture.
- [ ] Une source PipeWire explicite invalide fait échouer le check audio.
- [ ] Seuls le provider principal et le fallback configuré avec
  `health_enabled=true` reçoivent leur health check exact; aucun `/models`
  implicite ni health cleanup n'est émis.
- [ ] Une configuration `stdout`/clipboard `none`/UI `none` ne réclame ni
  `wl-copy`, ni `wtype`, ni D-Bus, ni OSD.
- [ ] Chaque sink et UI actif ajoute exactement ses dépendances conditionnelles.
- [ ] Un credential indisponible marque la santé correspondante `skipped` et ne
  déclenche aucune requête provider.

## Capture PipeWire

- [ ] La source par défaut capture le microphone attendu.
- [ ] `audio.source` sélectionne effectivement la source PipeWire demandée.
- [ ] L'indicateur d'écoute n'apparaît qu'après réception de vrais échantillons,
  pas à la seule création du WAV.
- [ ] Une capture sous le seuil utile termine en `no_speech` sans sink.
- [ ] Débrancher ou rendre indisponible la source termine proprement avec une
  erreur audio classifiée.
- [ ] Après stop, cancel, erreur et fermeture de session, aucun `pw-record` ni
  enfant de helper ne reste actif.
- [ ] Les captures sont supprimées par défaut.
- [ ] La rétention opt-in écrit des WAV `0600` dans un répertoire `0700` et
  respecte les limites de nombre et taille.
- [ ] Une annulation ne conserve aucun WAV, même avec la rétention active.

## Cycle de session et IPC

- [ ] `start` retourne après disponibilité du socket, puis `status` affiche
  `starting` ou `recording` sans course visible.
- [ ] Un second `start` est refusé et ne lance pas de deuxième capture.
- [ ] `stop` passe par arrêt, transcription, traitement et livraison, puis
  `status` revient à `idle` avec le dernier résultat.
- [ ] `cancel` avant livraison produit `cancelled` et aucun sink.
- [ ] `toggle` démarre au repos et arrête en `starting` ou `recording`.
- [ ] `toggle` est refusé pendant transcription, traitement ou livraison.
- [ ] `SIGINT`, `SIGTERM` et `SIGHUP` de la session suivent le chemin
  d'annulation et nettoient socket/verrou.
- [ ] `session.sock`, `session.lock` et `last-result.json` ont les permissions
  attendues et ne contiennent aucun transcript.
- [ ] Un client IPC `subscribe` reçoit l'accusé `subscribed`, puis des événements
  JSONL v1 sans transcript ni secret.
- [ ] Un abonné lent peut perdre des événements sans ralentir la session; un
  `status` permet de reconstruire l'état.
- [ ] Une requête IPC mal formée, une version inconnue et une commande inconnue
  sont refusées sans tuer le serveur.
- [ ] Un socket obsolète est remplacé au démarrage et supprimé à la terminaison.

## Commandes composables

- [ ] `transcribe` accepte un WAV PCM s16 mono 16 kHz et produit uniquement le
  transcript final sur stdout en mode texte.
- [ ] `transcribe` refuse fréquence, canaux, encodage et conteneur invalides.
- [ ] `cleanup` lit jusqu'à EOF, n'initialise pas le STT et recopie l'entrée si le
  cleanup est désactivé.
- [ ] `dictate --sink stdout` capture au premier plan et traite EOF comme un stop
  propre.
- [ ] `dictate --sink file` remplace atomiquement le fichier configuré en mode
  `0600`.
- [ ] SIGINT, SIGTERM et SIGHUP arrêtent proprement `dictate` puis déclenchent le
  traitement, contrairement à l'annulation d'une session détachée.
- [ ] `--json` produit exactement un objet conforme au schéma sur stdout en
  succès et en échec.
- [ ] `--events` réserve stderr aux événements JSONL et aucun flux ne contient
  de transcript inattendu.

## Sinks et presse-papiers

- [ ] `paste` colle une fois dans au moins une application GTK, une application
  Qt et un terminal Wayland.
- [ ] Avec des raccourcis configurés, `paste` émet uniquement le premier
  raccourci via `wtype`.
- [ ] L'échec technique de ce raccourci fait échouer la livraison sans frappe de
  repli.
- [ ] `paste` avec clipboard `none` frappe directement sans réutiliser un ancien
  presse-papiers.
- [ ] `type` traite correctement ASCII, accents, ponctuation et texte long.
- [ ] `type_newlines=space|marker|dangerous-keep` produit les trois rendus documentés et seul `dangerous-keep` injecte des LF.
- [ ] `clipboard` laisse le texte final disponible sans injection.
- [ ] `raw-and-clean` expose d'abord le STT brut puis le résultat final.
- [ ] `clean-only` n'expose jamais le STT brut.
- [ ] `none` ne lance jamais `wl-copy`.
- [ ] `transient` tente l'effacement après succès et échec des sinks supportés;
  le résultat avec un gestionnaire d'historique est noté explicitement.
- [ ] un mode effectif `paste` avec raccourci et clipboard `transient` est refusé
  avant capture; `type+transient` conserve l'ordre copie, frappe, clear.
- [ ] `start --clipboard-only` remplace un sink global `paste` ou `type` et ne
  produit aucune injection.
- [ ] `clipboard-only` refuse les politiques `none` et `transient` avant capture.
- [ ] `stdout` et `file` avec clipboard `none` fonctionnent sans `wl-copy`,
  `wtype`, notifications ni helper OSD.

## UI

- [ ] `notify` affiche les états démarrage, écoute, transcription, traitement,
  livraison et résultat terminal sans transcript.
- [ ] Les notifications successives remplacent la précédente et une panne D-Bus
  ne fait pas échouer la dictée.
- [ ] `osd` affiche les états actifs, pulse pendant l'écoute et disparaît après
  un résultat terminal selon son délai.
- [ ] Les positions `top-left`, `top-center`, `center` et les styles `nord`,
  `compact` sont visuellement utilisables avec l'échelle du bureau.
- [ ] L'OSD ne prend pas le focus, ne réserve pas de zone et reste au-dessus des
  fenêtres sans bloquer le clavier.
- [ ] Tuer `murmur-osd` vérifie les relances bornées et n'interrompt ni STT ni
  sink.
- [ ] `ui.mode=none` n'affiche rien et ne tente aucune connexion D-Bus/OSD.
- [ ] `ui.sound=true` joue un son à la readiness réelle puis à l'arrêt, y compris
  sur annulation; absence de lecteur ou de fichier reste best-effort et ne fait
  pas échouer la session.

## Providers, fallback et policy

- [ ] Un provider local ne produit aucune connexion hors loopback.
- [ ] Un provider LAN autorisé reçoit le WAV et devient le provider effectif.
- [ ] Une destination LAN hors policy est bloquée avant l'envoi du moindre
  octet.
- [ ] Un hostname résolu vers plusieurs adresses est refusé si l'une d'elles
  dépasse la zone autorisée.
- [ ] Les variables de proxy HTTP(S) n'influencent pas le transport.
- [ ] Une redirection du provider est refusée sans renvoi du WAV ou du secret.
- [ ] Un provider cloud HTTPS reçoit le bearer attendu sans exposition locale du
  secret.
- [ ] HTTP cloud est refusé sans `allow_insecure=true`.
- [ ] Une réponse 5xx peut être retentée dans la limite configurée; une requête
  partie sans réponse certaine n'est pas rejouée.
- [ ] L'échec du principal déclenche le fallback en réutilisant la même capture.
- [ ] L'échec des deux providers produit une erreur classifiée sans message brut
  sensible.
- [ ] Une panne cleanup conserve le meilleur texte local, ajoute un warning et
  permet la livraison.

## Chunking live

- [ ] `chunk_seconds=0` conserve le chemin sans découpage.
- [ ] Le chunking post-capture produit des segments ordonnés et fusionne une
  queue courte.
- [ ] En live, un seul `pw-record` reste actif pendant toute la capture.
- [ ] Le STT d'un segment commence avant le stop final lors d'une dictée assez
  longue.
- [ ] Le nombre de requêtes simultanées ne dépasse jamais `chunk_workers`.
- [ ] Des réponses terminant dans le désordre sont livrées dans l'ordre audio.
- [ ] Le PCM final ne présente ni trou ni duplication aux frontières.
- [ ] Cleanup, plugin et sink ne sont exécutés qu'une fois sur le texte assemblé.
- [ ] Sans opt-in, un échec de segment interdit toute livraison.
- [ ] Sans mélange autorisé, le fallback reprend l'ensemble des segments.
- [ ] `allow_partial=true` omet les segments échoués et trace le warning.
- [ ] `allow_mixed_providers=true` conserve le provider de chaque segment et
  trace le mélange.
- [ ] Stop et cancel pendant des requêtes live drainent le pool et ne laissent ni
  requête HTTP ni fichier temporaire actif.

## Pipeline et plugin Markdown

- [ ] Les commandes vocales françaises de guillemets, ligne et paragraphe ont le
  rendu documenté.
- [ ] Le pack anglais produit les guillemets anglais sans modifier le français.
- [ ] Le cleanup conserve exactement les tokens protégés; une altération produit
  un repli local avec warning.
- [ ] Sans déclencheur `commande stt`, les mots titre, liste, gras et italique
  restent du texte ordinaire.
- [ ] Le plugin Markdown rend titres 1/2, gras, italique, listes à puces et listes
  numérotées.
- [ ] Un titre contenant `Dr. Martin` ou de la ponctuation ordinaire reste entier
  jusqu'à `fin du titre`; une suite d'items `1, 3` est refusée sans renumérotation.
- [ ] Une commande Markdown inconnue, vide ou non fermée conserve intégralement
  le texte pré-plugin et ajoute un warning.
- [ ] Le timeout du plugin n'empêche pas la livraison du texte de repli.
- [ ] Une implémentation plugin `lan`/`internet` utilisant seulement l'interface
  locale est refusée; l'interface externe reçoit un HTTP netguard borné à sa zone.

## Hyprland et usage prolongé

- [ ] Les raccourcis Hyprland `toggle`, `cancel` et `clipboard-only` fonctionnent
  sans terminal visible et sans dépendre du répertoire courant.
- [ ] Une répétition rapide de raccourcis ne crée ni double session ni double
  livraison.
- [ ] Le focus reste dans l'application cible pendant UI, collage et frappe.
- [ ] Une dictée courte, une dictée longue et une série d'au moins vingt cycles
  ne laissent ni processus, socket, verrou ni fichier temporaire orphelin.
- [ ] Une fermeture de session graphique pendant une capture termine sans
  livrer tardivement dans la session suivante.
- [ ] Après reconnexion desktop, une nouvelle dictée démarre normalement.

## Résultats

### Automatisé

**Réussi le 29 juillet 2026.** Cette validation ne vaut pas validation desktop.

- [x] `make lint test build`: format Go, `go vet`, toute la suite avec `-race`
  et construction `-trimpath` réussis.
- [x] `make installcheck`: installation avec helper OSD, exécution du binaire
  installé et désinstallation dans un `DESTDIR` temporaire réussies.
- [x] Cycle séparé avec `PREFIX=/opt/murmur` et un `DESTDIR` temporaire:
  binaire/helper en `0755`, README en `0644`, exemples et docs présents,
  `murmur help` exécutable, puis tous les fichiers Murmur retirés par
  `make uninstall`.
- [x] Aucun chemin système réel n'a été utilisé comme destination de ces tests.

### Validation manuelle desktop

**Partiellement exécutée le 31 juillet 2026.** Le compte-rendu détaillé et le
rollback sont consignés dans
[`production-migration.md`](production-migration.md).

Validé sur Hyprland, PipeWire et le chemin de production avec provider LAN:

- `doctor` humain et JSON entièrement verts;
- capture foreground bornée sans clipboard ni injection;
- finalisation WAV et transcription réelle par le provider LAN;
- rendu des guillemets et paragraphes, puis copie clipboard-only;
- frappe `wtype` réelle dans un éditeur;
- collage réel et présence dans `cliphist`;
- notifications d'écoute, de traitement et de réussite;
- cleanup OpenRouter avec sentinelle protégée et sans fallback dégradé;
- refus d'un second démarrage concurrent;
- annulation terminale sans STT, sink ni processus orphelin;
- raccourcis canary puis principal testés physiquement.

Encore ouvert:

- dictée de plus de 60 secondes et chunking post-capture;
- vingt cycles quotidiens sans fuite de processus ou de fichiers;
- reprise après indisponibilité temporaire du provider LAN;
- couverture des autres applications prévues par cette checklist;
- reconnexion de session Wayland;
- OSD, son, live chunking, fallback cloud et plugin Markdown;
- tout test réel d'un provider cloud.

Les autres cases non cochées de cette checklist ne sont pas revendiquées comme
validées.
