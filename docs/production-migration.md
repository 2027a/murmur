# Migration de production

## État du 31 juillet 2026

Murmur est installé dans `~/.local` et constitue le chemin principal du bureau.
La configuration effective est `~/.config/murmur/config.toml`, suivie dans les
dotfiles sous `murmur/config.toml`.

Chemin de production validé:

- source PipeWire par défaut: RØDE NT-USB+ Mono;
- STT LAN: `http://192.168.100.33:18080/v1`, modèle `whisper-1`, français;
- cleanup OpenRouter actif avec `openai/gpt-4o-mini`;
- fallback STT cloud, live chunking et plugin Markdown désactivés;
- chunking post-capture à 60 secondes;
- sink `paste`, clipboard `raw-and-clean`, guillemets ASCII;
- notifications D-Bus;
- `Super+M`: toggle normal;
- `Super+Shift+M`: toggle clipboard-only;
- `Super+Échap`: annulation immédiate sans livraison.

Validations desktop réalisées:

- `doctor` humain et JSON entièrement verts;
- capture foreground bornée sans clipboard ni injection;
- finalisation WAV et transcription réelle par le provider LAN;
- rendu des guillemets et paragraphes, puis copie clipboard-only;
- frappe `wtype` réelle dans un éditeur;
- collage réel et présence dans `cliphist`;
- notifications écoute, traitement et réussite;
- cleanup OpenRouter validé avec sentinelle protégée et sans fallback dégradé;
- refus d'un second démarrage concurrent;
- annulation terminale sans STT, sink ni processus orphelin;
- raccourcis canary puis raccourci principal testés physiquement;
- benchmark GPT-4o mini contre GPT-5.6 Luna consigné dans
  [`cleanup-benchmark.md`](cleanup-benchmark.md);
- suite complète `go test -race ./...`, vet, build et installcheck après les
  corrections découvertes pendant les essais.

Deux défauts ont été trouvés et corrigés pendant la migration: prise en charge
d'un répertoire de configuration symlinké vers des dotfiles sûrs, et acceptation
du code de sortie 1 émis par `pw-record` après SIGINT lorsque le WAV final est
valide.

## Ancien Système

WaySTT ne possède plus aucun raccourci ni sous-mode actif dans la configuration
Hyprland. Ses fichiers restent installés uniquement comme archive temporaire;
la sauvegarde de configuration est
`~/.config/murmur.migration-backup-20260730`.

Les notifications Murmur utilisent les notifications D-Bus normales et des
catégories Dunst dédiées. Elles se remplacent sans prendre le focus: jaune au
démarrage, vert pendant l'écoute, cyan pendant le traitement, vert au succès,
orange à l'annulation et rouge à l'échec.

## Restant À Valider

- dictée réelle dépassant 60 secondes et chunking post-capture;
- vingt cycles quotidiens sans fuite de processus ou de fichiers;
- reprise après indisponibilité temporaire du provider LAN;
- reconnexion de session Wayland;
- OSD, son, live chunking, fallback cloud et Markdown avant toute
  activation de ces options.
