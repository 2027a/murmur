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
- le provider effectivement utilisé, ou `mixed` lorsque plusieurs providers ont
  contribué;
- les horodatages de début et de fin;
- les avertissements de dégradation structurés par `class`, `provider`,
  `component` et message contrôlé.

Avant d'exposer le socket, `session.MarkLastResultPending` remplace atomiquement
`last-result.session.json` en `0600` avec l'identifiant de la nouvelle session et
`pending=true`. Si cette invalidation échoue, notamment par permissions ou
`ENOSPC`, le démarrage échoue avant qu'une nouvelle session existe et l'ancien
résultat reste valide. Une fois le marqueur posé, un crash ou un échec de writer,
de renommage ou de synchronisation ne peut rendre l'ancien `session_id` courant:
le JSON précédent reste entier mais `session.ReadLastResult` le classe comme
périmé.

L'orchestrateur écrit le résultat atomique avant de fermer le socket et de
libérer le verrou. Après fermeture du journal et ajout éventuel du warning
MUR-019, `session.CommitLastResult` remplace atomiquement le marqueur avec
`pending=false`; ce commit est le seul point qui rend le résultat lisible par
`status`. L'erreur de l'écriture initiale reste suivie séparément de l'erreur
métier: le commit exige que cette écriture et l'éventuelle réécriture après
fermeture du journal aient toutes deux réussi. Une écriture, une réécriture ou
un commit raté laisse donc un diagnostic durable, sans fichier partiel. Le
marqueur validé reste dans le runtime: la session suivante le remplace, et le
nettoyage normal du runtime l'emporte avec les autres fichiers éphémères.

`session.ReadLastResult` lit d'abord le résultat puis le marqueur, rejette les
champs JSON inconnus et les résultats non terminaux. Cet ordre évite qu'une
lecture concurrente accepte l'ancien fichier après le remplacement du marqueur.
Un `last-result.json` sans marqueur, produit par une version antérieure, reste
lisible jusqu'au prochain démarrage. Les warnings STT et pipeline gardent la même
classe dans les événements `degraded` et dans `last-result.json`; aucune cause
brute n'est persistée.

## Orchestration

`session.Build` construit les dépendances depuis le snapshot de configuration
déjà résolu par le parent:

1. résolution des secrets STT, fallback et cleanup via
   `config.SecretResolver` et `sysexec.Executor`, avec l'environnement parent
   privé de tous les noms `api_key_env` du snapshot, providers inactifs compris;
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
le provider de chaque tentative réellement lancée, en capture live comme après
capture. `partial_started` est émis juste avant l'appel provider; `partial_done`
ou `partial_failed` conserve le même index original. Un arrêt anticipé, une
annulation ou un dépassement de budget ne produit aucun `partial_started` pour
les tentatives non lancées. Chaque reprise par fallback forme de nouvelles
tentatives identifiées par son propre provider. Le total reste omis tant que la
capture n'est pas terminée.

Lorsqu'un budget STT est dépassé en live, y compris pendant le stop et le
drainage des derniers segments, l'orchestrateur surveille le résultat STT,
annule le forwarder puis aborte la capture et attend sa terminaison. Le pool
STT ne détache aucun drain du canal de segments; le résultat terminal conserve
l'erreur et le provider de la tentative qui a franchi la limite.

La demande de stop emprunte un plan de contrôle distinct du transfert des
segments. Même si les workers, l'entrée du pool et la file du recorder sont tous
pleins, elle déclenche l'arrêt borné de `pw-record` sans attendre leur drainage.
La fermeture du flux et l'attente des workers restent séquentielles après cet
arrêt: elles peuvent retarder la transcription finale, mais jamais prolonger la
capture. La contre-pression n'ajoute aucune file non bornée et les index sont
assemblés sans duplication dans leur ordre d'origine.

Un échec non récupéré termine la session en `failed` avant le pipeline et le
sink. Les récupérations partielles et le mélange de providers sont deux opt-ins
indépendants. L'annulation ferme la capture, annule les requêtes HTTP, attend le
pool et effectue un seul cleanup global; aucun worker ni enfant `pw-record` ne
survit à `Run`.

Pendant `starting` et `recording`, l'orchestrateur observe aussi la terminaison
du recorder. Une sortie de `pw-record` non provoquée par stop ou cancel termine
immédiatement en `failed`/`audio_failed`, persiste le résultat et n'appelle jamais
le STT ni le sink en capture non-live, même si une demande de stop était déjà en
attente. Le recorder arbitre atomiquement la sortie et la prise en charge du stop:
une sortie observée la première ne peut donc pas transformer son WAV partiel en
capture réussie. Le stop et l'annulation pris en charge les premiers restent des
fins normales et ne sont pas reclassés à cause de la fermeture concomitante du
signal `Done`.

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
abandonne la capture et interdit tout effet de sink, y compris les copies
clipboard brute et finale. Une sortie vide ou en erreur du processing possède la
même garantie. Le passage atomique à `delivering` ferme cette fenêtre avant le
premier effet: une annulation tardive est refusée, puis `raw-and-clean` copie le
brut et le final dans cet ordre avant de poursuivre le sink. Les appels
concurrents ne peuvent donc produire qu'un résultat terminal et au plus une
livraison.

`SIGINT`, `SIGTERM` et `SIGHUP` suivent le même chemin que `cancel`. Les enfants
externes appartiennent à leurs propres groupes de processus et sont arrêtés par
`sysexec`; la session ferme ensuite l'UI, le socket et le verrou. Les événements
IPC/UI ne transportent jamais le transcript.

La fermeture UI intervient après la publication du terminal et en draine
explicitement la dernière remise. Les notifications annulent une notification
intermédiaire bloquée au profit du terminal et ferment leur client sous un budget
de 2,25 s. L'OSD écrit le terminal avant de fermer stdin, laisse le helper sortir
après son délai d'affichage configuré, puis le tue et le récolte au dépassement.
Sans terminal, les deux UI abandonnent immédiatement le travail intermédiaire;
l'OSD n'accorde que 250 ms à la sortie sur EOF. Aucun worker UI ni helper OSD ne
survit au retour de `Run`.

L'événement `provider_selected`, les événements globaux suivants et l'événement
terminal reprennent le provider effectif, ou `mixed` pour une transcription
multi-provider. Les événements de segment conservent leur provider propre.
L'événement terminal reprend aussi la classe de l'erreur qui a réellement
terminé la session. `cancelled` et `no_speech` restent des résultats terminaux
distincts et ne sont pas présentés comme `failed`.

## Journal borné

Le processus détaché prépare d'abord `murmur.log` dans le runtime privé, en
`0600`, puis redirige son `stderr` brut vers `/dev/null`. Après lecture du snapshot
validé, le logger JSONL ouvre son writer dédié et prend en charge la rotation
bornée. Les helpers ne peuvent donc pas injecter leur stderr dans ce fichier. Les
niveaux `debug`, `info`, `warn` et `error` ont des seuils distincts; les
composants et champs persistés sont fermés et les messages UI ne reprennent
jamais une cause brute. Une panne du logger est best-effort et ne rappelle pas le
logger.

Le processus interactif peut abonner un journal au bus avant le bootstrap. Le
fichier actif est `session.jsonl` dans le runtime Murmur résolu selon
[`configuration.md`](configuration.md); sa taille et le nombre
de rotations sont contrôlés par `[logging]`. `Close` draine la file bornée avant
de fermer le fichier. Les lignes utilisent le schéma fermé `events.Event`, qui
n'a aucun champ de transcript, secret, prompt ou corps provider. Comme tous les
abonnés du bus, le journal peut abandonner des événements sous surcharge et ne
constitue pas un audit exhaustif.

Le journal mémorise sa première erreur d'encodage, d'inspection, de rotation,
d'écriture, de réouverture ou de fermeture. Son callback de dégradation ne reçoit
que le nom fermé de l'opération: il ne repasse pas par le bus et ne peut donc ni
boucler sur le journal ni transporter transcript, prompt, corps HTTP ou secret.
Le logger et `last-result.json` utilisent des messages constants sans cause ni
chemin. `Close` draine les événements, retourne la première cause locale utile,
et la session ajoute alors un warning `internal/session-journal`. Une erreur
transitoire de `Stat`, `Rename`, `Write` ou réouverture ne désactive pas le
worker: le descripteur courant reste actif et les événements suivants retentent
la rotation puis l'écriture.

Le writer du journal événementiel et celui du log opérationnel sont deux
instances indépendantes avec des fichiers, limites et erreurs distincts. Cette
séparation est volontaire: le diagnostic d'une panne du journal, de l'OSD ou de
D-Bus ne dépend pas du canal événementiel qu'il doit diagnostiquer.
