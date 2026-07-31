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
l'API désactive les relances.

La fermeture est un échange de fin borné. Si un payload `terminal` a été remis à
`Show`, `Close` le conserve indépendamment de la saturation de la file, finit de
l'écrire, puis ferme stdin. Le helper dispose alors du plus grand délai terminal
configuré, augmenté d'une seconde de grâce, pour masquer la fenêtre et sortir de
lui-même. Au-delà, Murmur ferme le pipe, tue le groupe du helper et attend sa
récolte. Sans payload terminal, stdin est fermé immédiatement et cette grâce est
limitée à 250 ms: un helper qui ne réagit pas à EOF est ensuite tué. Aucun
processus ou goroutine d'attente OSD ne survit au retour de `Close`. Si
l'écriture terminale rencontre un helper mort ou un pipe déjà fermé pendant
`Close`, Murmur récolte d'abord ce processus puis tente une unique relance avec
le temps restant. Une sortie déjà observée après une écriture terminale réussie,
mais avant la fermeture volontaire de stdin, suit la même règle. Un helper encore
vivant qui traite le terminal puis sort après EOF n'est pas relancé; aucune
relance ne prolonge la deadline de fermeture.

## Payload

| Champ | Type | Obligation | Sens |
|---|---|---|---|
| `v` | entier | oui | vaut `1` |
| `state` | chaîne | oui | état d'exécution ou `terminal`/`error` |
| `visible` | booléen | oui | affiche ou masque l'OSD |
| `result` | chaîne | non | `completed`, `cancelled`, `no_speech`, `failed` |
| `error_class` | chaîne | non | classe fermée de `internal/errs` |
| `provider` | chaîne | non | provider effectif ou `mixed`, sans contenu transcrit |
| `partial` | entier | non | index 1-based du segment |
| `total` | entier | non | nombre de segments, connu après arrêt |

Les trois événements partiels décrivent des tentatives provider réelles. Pour un
même `partial` et un même `provider`, `partial_started` précède exactement un
`partial_done` ou `partial_failed`. Un fallback peut donc produire une nouvelle
tentative pour le même segment avec un autre provider. Aucun événement
`partial_started` n'est synthétisé pour un segment ignoré après un arrêt anticipé,
une annulation ou un dépassement de budget.

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
deux délais distincts. Après un terminal, EOF signifie que plus aucun état
n'arrivera, pas qu'il faut masquer immédiatement la fenêtre: le helper attend la
fin du timer terminal, masque l'OSD, puis quitte. EOF sans terminal le fait quitter
immédiatement.

Le schéma TOML expose `ui.osd.helper`, `position`, `success_timeout_ms` et
`error_timeout_ms`. La session transmet ces valeurs au helper; `doctor` cherche
le helper effectif dans `PATH` (ou vérifie son chemin) et exécute `<helper>
--check`. Le style reste une option directe du helper, non exposée par le TOML.
Aucune clé non documentée n'est acceptée.
