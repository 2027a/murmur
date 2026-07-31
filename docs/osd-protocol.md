# Protocole OSD v1

L'OSD n'utilise pas le socket IPC. Le cœur lance un helper local
`murmur-osd`, lui écrit un objet JSON par ligne sur stdin et ne lit aucune
réponse. Cette réduction volontaire du bus d'événements ne constitue pas une API
réseau et ne transporte jamais de transcript.

## Transport

```json
{"v":1,"state":"recording","visible":true}
```

`internal/ui.OSD` utilise une file bornée. `Show` ne bloque jamais: si la file
est pleine, l'état le plus ancien est abandonné au profit du plus récent. Un
worker écrit dans le pipe. La mort, le blocage ou l'absence du helper reste
best-effort et ne modifie jamais le résultat de session.

Le dernier payload connu est renvoyé après une relance. Le nombre de relances est
borné par `OSDOptions.MaxRestarts`, trois par défaut; une valeur négative dans
l'API désactive les relances. `Close` ferme le contexte, le pipe si possible et
tue le processus restant.

## Payload

| Champ | Type | Obligation | Sens |
|---|---|---|---|
| `v` | entier | oui | vaut `1` |
| `state` | chaîne | oui | état d'exécution ou `terminal`/`error` |
| `visible` | booléen | oui | affiche ou masque l'OSD |
| `result` | chaîne | non | `completed`, `cancelled`, `no_speech`, `failed` |
| `error_class` | chaîne | non | classe fermée de `internal/errs` |
| `provider` | chaîne | non | provider effectif, sans contenu transcrit |
| `partial` | entier | non | index 1-based du segment |
| `total` | entier | non | nombre de segments, connu après arrêt |

Les événements de session avec un état conservent cet état. `terminal` devient
`state="terminal"`, `delivered` devient `delivering`, et `degraded` ou
`partial_failed` devient `error`. Une implémentation doit ignorer une ligne JSON
invalide et une version inconnue.

Le payload n'a ni titre, ni corps, ni champ texte arbitraire. Le helper dérive
ses libellés de `state` et `result`. La couleur d'erreur est réservée à
`state="error"` et `result="failed"`; `cancelled` et `no_speech` restent des
terminaisons distinctes.

## Helper GTK

Le helper de référence `helpers/osd-gtk/murmur-osd.py` utilise GTK3 et
gtk-layer-shell. Installé avec `make install INSTALL_OSD=1`, il devient
`$(PREFIX)/bin/murmur-osd`. Ses dépendances Python/GTK sont volontairement
conditionnelles.

```text
murmur-osd [--position top-left|top-center|center]
           [--style nord|compact]
           [--success-timeout-ms 2500]
           [--error-timeout-ms 4000]
           [--check]
```

`--check` vérifie que Python peut importer GTK3 et gtk-layer-shell sans ouvrir
de fenêtre. L'exécution normale exige `WAYLAND_DISPLAY`. La fenêtre est placée
sur la couche overlay, ne réserve aucune zone, refuse le focus clavier et lit
stdin avec un descripteur non bloquant et un tampon limité à 64 Kio. Les états
actifs restent affichés; un résultat terminal non fautif et une erreur utilisent
deux délais distincts.

Le schéma TOML expose `ui.osd.helper`, `position`, `success_timeout_ms` et
`error_timeout_ms`. La session transmet ces valeurs au helper; `doctor` cherche
le helper effectif dans `PATH` (ou vérifie son chemin) et exécute `<helper>
--check`. Le style reste une option directe du helper, non exposée par le TOML.
Aucune clé non documentée n'est acceptée.
