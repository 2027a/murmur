# État d'implémentation

Ce relevé trace les jalons fonctionnels à partir des contrats et tests présents
dans le dépôt. « Implémenté » signifie que le code et des tests automatisés
existent; cela ne remplace pas la validation sur un bureau Wayland réel décrite
dans [`desktop-validation.md`](desktop-validation.md).

| Jalon | État | Livrables vérifiables |
|---|---|---|
| M0, fondations | Implémenté | chemins XDG privés, écritures atomiques, taxonomie d'erreurs, événements versionnés sans transcript, exécution de processus injectable |
| M1, configuration et policy | Implémenté | TOML strict, profils data Socle à défauts faibles, précédence testée, schémas STT/cleanup séparés, sources de secrets, zones local/LAN/internet et link-local, refus `policy_blocked` avant transfert |
| M2, session et IPC | Implémenté | machine d'état, verrou unique, dernier résultat, socket Unix v1, abonnement aux événements, bootstrap sans course |
| M3, capture audio | Implémenté | `pw-record`, WAV PCM canonique, readiness et mort prématurée détectées, stop/abort bornés, chunking et rétention privée |
| M4, transcription STT | Implémenté | STT OpenAI-compatible, health configurable partagé avec doctor, retries prudents, fallback et résultats segmentés |
| M5, pipeline texte | Implémenté | packs français/anglais embarqués, protection des intentions, cleanup dégradable, rendu déterministe et runtime de plugin |
| M6, livraison | Implémenté | clipboard, injection `wtype`, sinks paste/type/clipboard/stdout/file, livraison au plus une fois et clipboard-only |
| M7, diagnostics et UI | Implémenté | doctor conditionnel sans `/models` implicite, journal JSONL rotatif sans texte, logging consommé, barre QuickShell avec vumètre et repli automatique, OSD configurable, notifications et son best-effort |
| M8, assemblage et livraison | Implémenté; validation desktop partielle | CLI complète, ré-exécution de session, orchestration réelle, acceptation automatisée, packaging T8.3 et CI Forgejo; T8.4 partiellement exécutée le 31 juillet 2026 |

## Détail M8

| Tâche | État | Preuve |
|---|---|---|
| T8.1, câblage du produit | Implémenté | `cmd/murmur`, `internal/cli`, `internal/session.Build` et tests de processus réels |
| T8.2, acceptation Socle | Implémenté | scénarios documentés dans [`acceptance.md`](acceptance.md), sans réseau externe ni desktop requis |
| T8.3, packaging et documentation | Implémenté | `make install`, `uninstall`, `installcheck`, `PREFIX`, `DESTDIR`, helper OSD opt-in, docs et exemples installés |
| T8.4, validation desktop | Partiellement exécutée le 31 juillet 2026 | périmètre validé et points ouverts dans [`desktop-validation.md`](desktop-validation.md); compte-rendu dans [`production-migration.md`](production-migration.md) |

### Périmètre T8.4 au 31 juillet 2026

Le chemin de production effectivement validé couvre:

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

Restent ouverts:

- dictée de plus de 60 secondes et chunking post-capture;
- vingt cycles quotidiens sans fuite de processus ou de fichiers;
- reprise après indisponibilité temporaire du provider LAN;
- couverture des autres applications prévues par la checklist;
- reconnexion de session Wayland;
- OSD, son, live chunking, fallback cloud et plugin Markdown;
- tout test réel d'un provider cloud.

La checklist complète et son bilan partiel sont dans
[`desktop-validation.md`](desktop-validation.md). Le détail de l'environnement,
des essais et du rollback est consigné dans le compte-rendu daté
[`production-migration.md`](production-migration.md).

## Extensions intégrées

| Extension | État | Contrat |
|---|---|---|
| CLI composable | Implémenté | `transcribe`, `cleanup`, `dictate`, résultat JSON v1 et événements JSONL |
| Chunking live | Implémenté | snapshots d'un unique recorder, pool borné, ordre restauré, queue PCM fusionnée |
| Récupération segmentée | Implémenté, opt-in | `allow_partial` et `allow_mixed_providers`, warnings et provenance par segment |
| Pack anglais | Implémenté | intentions communes, typographie anglaise et indépendance du pack français |
| Plugin Markdown | Implémenté, opt-in | fermetures lexicales, numéros d'items validés, capability netguard externe, timeout et repli sans tokens |
| Helper OSD GTK | Implémenté, installation opt-in | protocole JSONL v1, helper/position/délais configurables, file non bloquante, relances bornées, layer-shell sans focus |
| Barre QuickShell (#51) | Implémentée, déployée et validée par Mathieu | `auto`/`quickshell`, RMS/crêtes sur la capture existante, instantanés acquittés, repli notifications et reprise après reload; rendu Nord adaptatif dans les dotfiles, détails et limites dans [`quickshell.md`](quickshell.md) |

## Intégration continue

Le workflow Forgejo [`.forgejo/workflows/ci.yml`](../.forgejo/workflows/ci.yml)
exécute sur un runner natif linux/amd64, avec Go 1.23.12 et 1.26.5, `go mod
verify`, `make lint`, `go test -race ./...`, `make build` et `make
installcheck`. Les tests Python du helper OSD s'exécutent aussi avec des modules
GTK factices et n'exigent donc que la bibliothèque standard. Le binaire installé
est réellement exécuté sur amd64. Le cache Actions est désactivé, les caches Go
de compilation et de tests sont purgés, puis les modules verrouillés sont rendus
disponibles et vérifiés. Les étapes shell neutralisent explicitement les jetons
automatiques `FORGEJO_TOKEN` et `GITHUB_TOKEN`; les contrôles utilisent ensuite
`GOPROXY=off` et n'exigent ni desktop ni service réseau externe.

Pour chaque version Go, un job distinct cross-compile aussi le binaire
linux/arm64 avec `CGO_ENABLED=0` et vérifie les métadonnées de l'exécutable. Il
ne l'exécute pas et ne prétend donc pas couvrir le détecteur de courses sur
arm64. Aucun label de runner arm64 natif n'est publiquement consultable sur la
forge; cette séparation évite de masquer une émulation QEMU fragile derrière
une matrice d'architectures.

Tous les jobs ont une limite globale et chaque commande longue a son propre
délai. Aucun échec n'est toléré et aucun résultat de commande n'est restauré
depuis un cache. Pour rendre ces statuts strictement bloquants à la fusion, la
protection de la branche Forgejo doit exiger les quatre jobs de la matrice.

## Limite de validation

Les tests automatisés utilisent des serveurs HTTP, exécutables, recorders,
sinks, D-Bus et sockets factices ou temporaires. Ils couvrent les contrats sans
supposer un desktop installé. La validation T8.4 du 31 juillet 2026 couvre le
chemin de production décrit ci-dessus. Les variantes, applications et fonctions
listées comme ouvertes restent à valider manuellement; les tests automatisés ne
les couvrent pas.
