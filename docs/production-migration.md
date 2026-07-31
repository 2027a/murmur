# Migration de production

## État du 31 juillet 2026

Murmur est installé dans `~/.local` et constitue le chemin principal du bureau.
La configuration effective est `~/.config/murmur/config.toml`, suivie dans les
dotfiles sous `murmur/config.toml`.

Chemin de production validé:

- source PipeWire par défaut: RØDE NT-USB+ Mono;
- STT LAN: `http://192.168.100.33:18080/v1`, modèle `whisper-1`, français;
- cleanup, fallback cloud, live chunking et plugin Markdown désactivés;
- chunking post-capture à 60 secondes;
- sink `paste`, clipboard `raw-and-clean`, guillemets ASCII;
- notifications D-Bus;
- `Super+M`: toggle normal;
- `Super+Shift+M`: toggle clipboard-only;
- `Super+Ctrl+Échap`: annulation.

Validations desktop réalisées:

- `doctor` humain et JSON entièrement verts;
- capture foreground bornée sans clipboard ni injection;
- finalisation WAV et transcription réelle par le provider LAN;
- rendu des guillemets et paragraphes, puis copie clipboard-only;
- frappe `wtype` réelle dans un éditeur;
- collage réel et présence dans `cliphist`;
- notifications écoute, traitement et réussite;
- refus d'un second démarrage concurrent;
- annulation terminale sans STT, sink ni processus orphelin;
- raccourcis canary puis raccourci principal testés physiquement;
- suite complète `go test -race ./...`, vet, build et installcheck après les
  corrections découvertes pendant les essais.

Deux défauts ont été trouvés et corrigés pendant la migration: prise en charge
d'un répertoire de configuration symlinké vers des dotfiles sûrs, et acceptation
du code de sortie 1 émis par `pw-record` après SIGINT lorsque le WAV final est
valide.

## Rollback

L'ancien système reste installé sans être le chemin principal:

- `Super+Alt+M`: ancien WaySTT normal;
- `Super+Alt+Shift+M`: ancien WaySTT clipboard-only;
- sauvegarde de configuration:
  `~/.config/murmur.migration-backup-20260730`.

Le rollback ne doit être supprimé qu'après une période d'usage couvrant les
dictées longues, les applications habituelles et une reconnexion Wayland.

## Restant À Valider

- dictée réelle dépassant 60 secondes et chunking post-capture;
- vingt cycles quotidiens sans fuite de processus ou de fichiers;
- reprise après indisponibilité temporaire du provider LAN;
- reconnexion de session Wayland;
- OSD, son, live chunking, cleanup, fallback cloud et Markdown avant toute
  activation de ces options.
