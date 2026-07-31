# Suite d'acceptation Socle

La tâche T8.2 est couverte sans service desktop ni réseau externe. Les tests
emploient `httptest`, des recorders/sinks factices, des exécutables temporaires
et des sockets Unix sous `t.TempDir()`.

| Spec §17 | Test | Chemin vérifié |
|---|---|---|
| 01 | `internal/acceptance.TestAcceptance_01_LANNominalCleansAndPastesOnce` | capture, STT LAN, cleanup LAN, presse-papiers et collage unique |
| 02 | `internal/acceptance.TestAcceptance_02_LocalOnlyNeverContactsInternetProvider` | STT et cleanup locaux; provider Internet non contacté |
| 03 | `internal/acceptance.TestAcceptance_03_CloudReceivesCredentialsAndIsEffectiveProvider` | credential HTTP et provider effectif persisté |
| 04 | `internal/acceptance.TestAcceptance_04_PolicyBlocksFallbackBeforeAnyByte` | fallback Internet actif refusé depuis une vraie `config.Config`, avant construction HTTP et `Accept`, erreur `policy_blocked` |
| 05 | `internal/acceptance.TestAcceptance_05_FallbackReusesCaptureAudio` | une capture, corps WAV identique pour principal et fallback |
| 06 | `internal/acceptance.TestAcceptance_06_CleanupFailureDeliversDeterministicFallbackAndDegradedEvent` | repli déterministe, livraison et événement `degraded` |
| 07 | `internal/acceptance.TestAcceptance_07_LiveSnapshotsReconcatenateWithoutSampleLoss` | snapshots pendant capture, fusion de queue et reconcaténation PCM exacte |
| 08 | `internal/acceptance.TestAcceptance_08_LiveWorkersAreBoundedOrderedAndCancellable` | pool borné, ordre inverse, échec sans sink et annulation drainée |
| 09 | `internal/session.TestAcceptance_09_CancelBeforeDeliveringNeverCallsSink` | annulation coopérative d'un job, aucun sink, résultat `cancelled` |
| 10 | `internal/acceptance.TestAcceptance_10_ClipboardOnlyOverridesGlobalPaste` | priorité de session sur le sink global `paste` |
| 11 | `internal/acceptance.TestAcceptance_11_EmptyTranscriptProducesNoSpeechWithoutSink` | réponse STT vide, `no_speech`, aucun fichier ni commande externe |
| 12 | `internal/acceptance.TestAcceptance_12_NoUIOrDesktopToolsRequiredForStdout` | stdout et CLI composable sans UI, presse-papiers ni compositor; subprocess `transcribe`, `cleanup` et `dictate`, JSON/JSONL, stdin et sinks foreground |
| 13 | `internal/acceptance.TestAcceptance_13_OSDFailureDoesNotStopTranscriptionOrSink` | échec du helper OSD, session et sink terminés normalement |
| 14 | `internal/doctor.TestAcceptance_14_DoctorIsStrictlyConditional` | checks dérivés uniquement de la configuration utilisée |
| 17 | `internal/acceptance.TestAcceptance_17_SegmentFailureRestartsFallbackWithoutFalseProvider` | reprise intégrale des segments et provider effectif non mixte |
| 18 | `internal/acceptance.TestAcceptance_18_EnglishCommandsMatchConfiguredTypography` | commandes anglaises via STT, pipeline et sink |
| 19 | `internal/textpipe.TestAcceptance_19_PacksRemainIndependent` | extension anglaise sans changement du rendu français |
| MUR-001 | `internal/acceptance.TestAcceptance_MUR001_UploadUsesValidatedBytesWithAndWithoutChunking` | substitution atomique neutralisée, suffixe hors RIFF exclu et PCM segmenté exact |
| MUR-015 | `internal/acceptance.TestAcceptance_MUR015_MixedProviderProvenanceIsGlobalAndOrderIndependent` | provenance globale `mixed` en batch/live et détail provider conservé dans les deux ordres |

Les tests comportementaux complémentaires couvrent les retries transmis par
`core`, le health endpoint exact de `doctor`, les profils et leur précédence, le
rendu `ascii_quotes`, le classement link-local LAN, le schéma cleanup séparé et
la rotation du journal JSONL.

Les tests MUR-001 complémentaires vérifient la politique fichier régulier
(symlink final, FIFO et device refusés), le multipart borné au `memfd` RIFF
scellé, l'immunité à une réécriture `WriteAt` du même inode et la
fermeture/suppression des sources, segments et répertoires sur une erreur
provider.

`TestBinaryProcessStartReexecSocketStopAndCancel` construit le vrai binaire puis
vérifie deux cycles complets. `start` ré-exécute `__session`, expose le socket,
un faux `pw-record` produit un WAV canonique, puis `stop` passe par le faux
provider HTTP et écrit le sink fichier. Le second cycle envoie `cancel` avant
STT et vérifie le résultat terminal. Le test attend aussi la disparition du
socket, ce qui couvre la fermeture du serveur et la libération du processus.

Le sous-test `composable subprocesses` du scénario 12 construit aussi le vrai
binaire. Avec `PATH` vide, il vérifie `transcribe` et `cleanup` contre un serveur
`httptest`, le chunking, stdin/stdout, le contrat JSON, les événements JSONL et
l'erreur WAV `audio_failed`. Un faux `pw-record`, sans outil desktop, couvre
ensuite `dictate` avec son sink `stdout` par défaut, son arrêt par signal
synchronisé sur la lecture effective de stdin, et `dictate --sink file`. Le
schéma JSON versionné est interprété et appliqué aux résultats représentatifs par
`internal/cli.TestCommandResultFixtures` et
`internal/cli.TestRealComposableCommandResultsConformToContract`.

Les tests `internal/audio/pwrecord.TestLiveSingleProcessGrowingWAVHasExactPCMAndMergesTail`
et `TestLiveAbortLeavesNoProcessOrCaptureWithRetention` utilisent exclusivement
`sysexec.FakeExecutor`. Ils font croître un WAV par appends successifs, vérifient
un unique `pw-record`, l'égalité PCM exacte après segmentation, la fusion du
dernier segment, le cleanup idempotent et l'arrêt de l'enfant sur cancel.
