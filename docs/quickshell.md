# UI de barre QuickShell

Conception issue du [ticket 51](https://git.2027a.net/2027a/murmur/issues/51).
Les mesures et le transport appartiennent à Murmur; le rendu local appartient
aux dotfiles `quickshell/mathieu`, sans copie QML dans ce dépôt.

## Sélection

```toml
[ui]
mode = "auto"
sound = true
```

- `auto` : barre si le module QML répond, notifications sinon. Le retour de la
  barre retire la notification de repli et reprend l'instantané courant.
- `quickshell` : barre seule. Module absent ou bloqué : avertissement dans le
  log UI, échec du check `doctor`, capture et livraison indépendantes.
- `notify` : notifications seules, sans mesure de niveau supplémentaire.
- `osd` et `none` restent disponibles. Le défaut générique reste `notify`.

Installer QuickShell ou constater son processus ne suffit pas. La détection
vérifie la réponse du code QML, avec un délai de 600 ms par échange. Le son
reste indépendant du frontend. Les étapes de construction des providers sont
également affichées en `starting`, avant la disponibilité du micro.

## Pont et instantanés

Le shell lance **un seul** `murmur ui`, via un `Process` avec `stdinEnabled: true`
et un `SplitParser` sur stdout. Le pont écoute `ui.sock` (0600) dans le runtime
privé Murmur, sous verrou `ui.lock`. La fermeture de stdin termine le pont;
une nouvelle instance récupère le socket abandonné sous le même verrou.
Un stdout bloqué termine le pont après 600 ms et libère son verrou, afin qu'un
rechargement du shell puisse lancer une nouvelle instance.

Chaque ligne stdout est un instantané complet, versionné `v=1` :

```json
{"v":1,"seq":42,"session_id":"identifiant","state":"recording","delivery":"paste","cleanup":true,"capture_ms":1800000000000,"level":{"rms":0.12,"peak":0.4,"clipping":false,"updated_ms":1800000000050}}
```

Après validation/application, le module écrit `42\n` sur stdin. Une ligne avec
`probe=true` est acquittée sans changer le dessin (`doctor`). Les séquences
sont attribuées par le pont; un ancien acquittement ne valide pas une nouvelle
trame. Le protocole client/pont utilise une connexion Unix par échange et un
octet `0x01` en réponse après acquittement QML.

Champs supplémentaires : `result`, `error_class`, `warning` (composant/classe
contrôlés), `finished_ms`. La destination `delivery` distingue notamment
`clipboard` de `paste`. Les fins `completed`, `cancelled`, `no_speech`, `failed`
utilisent `state=idle`. Une dégradation reste attachée au résultat final.

L'émetteur conserve un seul instantané remplaçable, scellé au résultat terminal.
`Show` et la réception des niveaux n'effectuent aucun I/O. Le worker réseau
émet au plus 20 rafraîchissements acoustiques/s, plus les transitions d'état,
et un heartbeat environ toutes les 250 ms pendant le traitement. Pas de polling
du statut ou de nouveau processus par échantillon. La fermeture d'une session
termine l'échange en cours et tente une dernière émission bornée du résultat.

Après redémarrage du shell pendant une dictée, l'instantané restaure son état,
la durée depuis `capture_ms` et les nouvelles mesures. Le module ne présente
plus un ancien état d'écoute après deux secondes sans trame. L'historique
visuel des niveaux se reconstruit; il n'est pas persisté.

Le journal d'événements existant n'est pas alimenté par les 20 mesures/s. Aucun
PCM, texte dicté, prompt ou secret n'est envoyé au shell.

## Mesure

En modes `auto`/`quickshell`, une goroutine inspecte le WAV déjà écrit par
`pw-record`, sur le même inode ouvert, toutes les 50 ms. Elle lit au plus les
100 dernières ms de PCM nouveau (3 200 octets), sans seconde capture, sans
snapshot audio complet et sans file d'attente à rattraper. Cette mesure
fonctionne en capture batch et live; elle ne dépend pas du worker STT.

- RMS et crête linéaires bornés entre 0 et 1;
- saturation constatée sur les échantillons à pleine échelle;
- `updated_ms` avance uniquement à la réception de nouveaux échantillons;
- affichage QML en échelle logarithmique de -60 à 0 dBFS et lissage de descente;
- signal faible après 3 s sous environ -50 dBFS; absence de données récente
  distinguée du silence mesuré après 1,5 s.

Un faible signal n'est pas une preuve de micro muet, ni le bruit une preuve de
parole. Il n'y a ni VAD ni arrêt automatique. La visualisation de niveau cesse
dès la demande d'arrêt; les mesures tardives ne réactivent jamais le micro.

## Rendu local

Dans les dotfiles : `MurmurService.qml` partage le pont, `MurmurIndicator.qml`
dessine le module et `MurmurLogic.js` définit les libellés/couleurs. Les douze
barres représentent un historique d'amplitude, pas des fréquences.

Le module fait 24 px de large au repos et 18 px de haut. Pendant une session,
sa largeur suit le texte affiché ou le vumètre avec sa durée. Il est placé à
gauche du quota Codex, après les éléments du tiroir, dans le cartouche droit : fond
bleu `#81a1c1`, icône sombre `#3b4252`, coins externes de 4 px gérés par le parent.
Fonds Nord mauve `#b48ead` en écoute, transcription et correction, vert en succès, jaune en
annulation/dégradation, rouge en erreur. Le mouvement de traitement est un
balayage, pas une estimation de progression.

Succès/annulation restent développés 2 s, avertissement/erreur 6 s. Un témoin
d'erreur reste ensuite jusqu'à consultation ou nouvelle session. Survol :
détails; clic gauche : épingler/fermer les détails et acquitter le témoin.
Les raccourcis restent le moyen de démarrer/arrêter/annuler; aucun vol de focus.

Chaque barre visible reflète le même état. Si elle est masquée, celle de
l'écran choisi au démarrage se révèle temporairement en overlay, sans zone
exclusive supplémentaire et sans modifier les choix de masquage. L'écran
focalisé prend le relais si celui du démarrage est débranché.

## Vérification

Mathieu a validé l'interface en usage réel après les derniers ajustements :
bleu habituel pendant l'écoute, position à gauche du quota Codex et largeur
adaptée au contenu. Il a demandé la clôture du ticket 51 sur ce périmètre.
La configuration locale déployée utilise `ui.mode="auto"` et `ui.sound=true`.
Après cette validation, Mathieu a choisi le mauve Nord `#b48ead` pour le fond
d'enregistrement; le repos conserve le bleu habituel.

```sh
make lint test build
MURMUR_UI_MODE=auto murmur doctor --json
quickshell ipc -c mathieu call murmur status
python3 helpers/quickshell-preview.py
# Captures de la barre seule, sans capture micro ni texte livré :
python3 helpers/quickshell-preview.py --screenshots /tmp/opencode --geometry '3000,0 840x24'
```

Le script de prévisualisation refuse une dictée déjà active. Il couvre les
états, silence, saturation, résultat dégradé et erreur; ses niveaux sont
synthétiques. Dans les dotfiles : `node quickshell/mathieu/murmur.test.cjs` et
`/usr/lib/qt6/bin/qmllint --max-warnings 0 MurmurService.qml MurmurIndicator.qml`.

Les tests Go vérifient RMS/crêtes connus, absence de relecture du son ancien,
mesure bornée, arrêt de mesure, nécessité de l'ACK QML, reprise d'instantané,
perte du frontend pendant un traitement sans nouveaux événements, conservation
du terminal et absence de blocage sous consommateur lent.

Validation locale : prévisualisation des états à 20 trames/s et captures de
barre; RMS/crêtes sur capture PipeWire réelle; barre masquée révélée sans
changement de réservation ni de focus; pont UI suspendu puis repris pendant
l'écoute; rechargement QuickShell pendant la capture. Ces captures réelles
ont été annulées avant toute livraison. Les scénarios de débranchement
physique du micro/écran et de plein écran restent à essayer manuellement.

Sur deux écrans, la prévisualisation locale a transmis 406 trames en 20,5 s,
avec ACK moyen de 1 ms et maximum de 10,4 ms. Après limitation des animations
à 20 Hz, le processus QuickShell entier a consommé environ 1 seconde CPU sur
ces 20,5 s (mesure `ps` à la seconde, tous modules confondus, pas un budget
isolé Murmur). Le premier rendu animé à 60 Hz consommait environ 3 secondes.

Lors des vérifications, `TestLiveStopDrainsAfterFatalSTT/fatal_segment_count`
a échoué une fois sur l'attente du premier segment pendant des essais en
parallèle. Le test a ensuite passé cinq exécutions isolées et la suite complète.
