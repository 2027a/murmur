# Primitives de session

## Machine d'état

`session.Machine` démarre dans `idle`. L'orchestrateur émet les commandes
internes dans cet ordre nominal:

```text
start
capture_ready
stop
capture_stopped
transcription_done
processing_done
delivery_done
```

Les états traversés sont `starting`, `recording`, `stopping`, `transcribing`,
`processing`, `delivering`, puis `idle` avec le résultat `completed`.

Cas particuliers contractuels:

- `stop` ou `toggle` pendant `starting` pose `StopPending`; `capture_ready`
  passe alors directement à `stopping`;
- `cancel` produit `cancelled` de `starting` à `processing`;
- `cancel` pendant `delivering` est refusé sans altérer le résultat futur;
- `toggle` démarre depuis `idle`, arrête depuis `starting` ou `recording`, et est
  refusé durant les autres états actifs;
- `no_speech` termine depuis `processing` sans passer par `delivering`;
- `no_speech` termine aussi depuis `stopping` pour une capture trop courte et
  depuis `transcribing` pour une réponse STT vide;
- `fail` produit `failed` depuis tout état actif;
- toute autre paire état/commande retourne une erreur classifiée.

Chaque transition effective publie exactement un événement. La demande de stop
différée ne change pas d'état et ne publie donc pas d'événement.

## Unicité

`session.AcquireLock(runtimeDir)` prend un `flock` exclusif non bloquant sur
`session.lock`. `start` le prend avant le fork, et le processus `__session`
conserve le descripteur jusqu'à sa sortie. Le fichier est ouvert avec
`O_NOFOLLOW|O_CLOEXEC`, puis validé par `fstat`: fichier régulier, un seul lien,
UID courant et mode `0600`.

Pour le transfert sans course, `start` passe `lock.File()` dans
`exec.Cmd.ExtraFiles`, appelle `lock.Detach()` après le démarrage réussi, et
`__session` enveloppe le descripteur reçu avec `session.AdoptLock`, qui reprend
un `LOCK_EX|LOCK_NB` et vérifie la cible du FD (ou le chemin attendu lorsqu'il
est fourni). `AdoptLock` remet immédiatement `FD_CLOEXEC`, avant toute inspection,
afin qu'aucun helper ou descendant ne prolonge accidentellement la durée du
verrou. Il ne faut pas
appeler `Close` côté parent après le fork: son `LOCK_UN` libérerait aussi le
verrou hérité.

Un conflit retourne `unavailable` avec le message `une session est déjà active`.

## Dernier résultat

`session.WriteLastResult` persiste atomiquement `last-result.json` en mode
`0600`. Le document versionné contient:

- l'identifiant et le résultat terminal;
- l'erreur classifiée éventuelle, sans cause brute;
- le provider effectivement utilisé;
- les horodatages de début et de fin;
- les avertissements de dégradation.

L'orchestrateur écrit ce résultat avant de fermer le socket et de libérer le
verrou. `session.ReadLastResult` rejette les champs JSON inconnus et les
résultats non terminaux.

## Orchestration

`session.Build` construit les dépendances depuis le snapshot de configuration
déjà résolu par le parent:

1. résolution des secrets STT, fallback et cleanup via
   `config.SecretResolver` et `sysexec.Executor`;
2. transports réseau bornés par la zone et les CIDR effectifs;
3. chaîne STT principal/fallback, pipeline `protect-cleanup-render`, sink
   enveloppé par `sink.Once`, UI et recorder `pw-record`;
4. découpage WAV selon `stt.chunk_seconds`, sans mélange implicite des
   providers;
5. suppression des segments et libération de la capture après le dernier appel
   STT.

La policy des providers STT principal, fallback et cleanup actif est revérifiée
au début de leur construction, avant résolution des credentials et avant
capture. Une zone active interdite retourne `policy_blocked`; le netguard refait
ensuite la classification de chaque destination effective avant tout octet HTTP.

Avant cette construction, `__session` vérifie que les trois numéros de FD sont
distincts, que le verrou est régulier, que le handshake et le snapshot sont deux
pipes distincts, puis fait correspondre le verrou au chemin
`<runtime>/session.lock`. Le snapshot JSON est limité à 1 MiB et tout rejet est
classifié `config_invalid` ou `internal` selon qu'il concerne le document ou les
descripteurs.

Lorsque `stt.live_chunking` est actif, un unique `pw-record` reste actif jusqu'au
stop. Des snapshots alignés du PCM physiquement présent dans son WAV alimentent
le pool STT borné pendant que le fichier continue de croître. Le stop finalise
ce recorder, relit le header final, extrait le reliquat depuis le dernier curseur,
ferme l'entrée du pool, attend tous les workers et réordonne les résultats avant
d'exécuter une seule fois le pipeline cleanup/plugin et le sink. Les événements
`partial_started`, `partial_done` et `partial_failed` portent l'index 1-based et
le provider de chaque tentative. Le total reste omis tant que la capture n'est
pas terminée.

Un échec non récupéré termine la session en `failed` avant le pipeline et le
sink. Les récupérations partielles et le mélange de providers sont deux opt-ins
indépendants. L'annulation ferme la capture, annule les requêtes HTTP, attend le
pool et effectue un seul cleanup global; aucun worker ni enfant `pw-record` ne
survit à `Run`.

Pendant `starting` et `recording`, l'orchestrateur observe aussi la terminaison
du recorder. Une sortie de `pw-record` non provoquée par stop ou cancel termine
immédiatement en `failed`/`audio_failed`, persiste le résultat et n'appelle jamais
le sink. Le stop et l'annulation demandés restent des fins normales et ne sont
pas reclassés à cause de la fermeture concomitante du signal `Done`.

Après adoption du verrou, `session.Bootstrap` fait entrer l'unique machine d'état
dans `starting` et l'expose immédiatement sur le socket. La résolution bornée des
secrets et `session.Build` viennent ensuite. Le relais répond à `status`, retient
`stop`/`toggle` et annule le contexte de construction sur `cancel`; après
attachement, il délègue à l'orchestrateur qui partage la même machine.

L'orchestrateur attend la readiness réelle de `pw-record`, puis traite une
demande `stop` reçue pendant le bootstrap ou le démarrage immédiatement après
cette readiness. C'est le comportement D15: la capture est finalisée proprement,
jamais tuée au milieu de son en-tête WAV. Un helper `api_key_command` annulé ou
expiré est tué avec tout son groupe de processus.

Une annulation de `starting` à `processing` annule le contexte des opérations,
abandonne la capture et interdit tout appel au sink. Le passage atomique à
`delivering` ferme cette fenêtre: une annulation tardive est refusée et la
livraison en cours termine. Les appels concurrents ne peuvent donc produire
qu'un résultat terminal et au plus une livraison.

`SIGINT`, `SIGTERM` et `SIGHUP` suivent le même chemin que `cancel`. Les enfants
externes appartiennent à leurs propres groupes de processus et sont arrêtés par
`sysexec`; la session ferme ensuite l'UI, le socket et le verrou. Les événements
IPC/UI ne transportent jamais le transcript.

L'événement terminal reprend le provider effectif et la classe de l'erreur qui a
réellement terminé la session. `cancelled` et `no_speech` restent des résultats
terminaux distincts et ne sont pas présentés comme `failed`.

## Journal borné

Le processus interactif peut abonner un journal au bus avant le bootstrap. Le
fichier actif est `$XDG_RUNTIME_DIR/murmur/session.jsonl`; sa taille et le nombre
de rotations sont contrôlés par `[logging]`. `Close` draine la file bornée avant
de fermer le fichier. Les lignes utilisent le schéma fermé `events.Event`, qui
n'a aucun champ de transcript, secret, prompt ou corps provider. Comme tous les
abonnés du bus, le journal peut abandonner des événements sous surcharge et ne
constitue pas un audit exhaustif.
