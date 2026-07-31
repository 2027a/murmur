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
| M7, diagnostics et UI | Implémenté | doctor conditionnel sans `/models` implicite, journal JSONL rotatif sans texte, logging consommé, OSD configurable, notifications et son best-effort |
| M8, assemblage et livraison | Implémenté; validation desktop ouverte | CLI complète, ré-exécution de session, orchestration réelle, acceptation automatisée, packaging T8.3; checklist T8.4 préparée mais non exécutée |

## Détail M8

| Tâche | État | Preuve |
|---|---|---|
| T8.1, câblage du produit | Implémenté | `cmd/murmur`, `internal/cli`, `internal/session.Build` et tests de processus réels |
| T8.2, acceptation Socle | Implémenté | scénarios documentés dans [`acceptance.md`](acceptance.md), sans réseau externe ni desktop requis |
| T8.3, packaging et documentation | Implémenté | `make install`, `uninstall`, `installcheck`, `PREFIX`, `DESTDIR`, helper OSD opt-in, docs et exemples installés |
| T8.4, validation desktop | Préparé, non exécuté | checklist exhaustive dans [`desktop-validation.md`](desktop-validation.md); cases manuelles volontairement ouvertes |

## Extensions intégrées

| Extension | État | Contrat |
|---|---|---|
| CLI composable | Implémenté | `transcribe`, `cleanup`, `dictate`, résultat JSON v1 et événements JSONL |
| Chunking live | Implémenté | snapshots d'un unique recorder, pool borné, ordre restauré, queue PCM fusionnée |
| Récupération segmentée | Implémenté, opt-in | `allow_partial` et `allow_mixed_providers`, warnings et provenance par segment |
| Pack anglais | Implémenté | intentions communes, typographie anglaise et indépendance du pack français |
| Plugin Markdown | Implémenté, opt-in | fermetures lexicales, numéros d'items validés, capability netguard externe, timeout et repli sans tokens |
| Helper OSD GTK | Implémenté, installation opt-in | protocole JSONL v1, helper/position/délais configurables, file non bloquante, relances bornées, layer-shell sans focus |

## Limite de validation

Les tests automatisés utilisent des serveurs HTTP, exécutables, recorders,
sinks, D-Bus et sockets factices ou temporaires. Ils couvrent les contrats sans
supposer un desktop installé. Les interactions réelles avec PipeWire, Wayland,
Hyprland, les applications cibles, les gestionnaires de presse-papiers, D-Bus,
gtk-layer-shell et des providers distants restent à valider manuellement en T8.4.
