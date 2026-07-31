# Providers

## STT OpenAI-compatible

`internal/stt.OpenAICompatible` envoie un `POST multipart/form-data` vers
`{base_url}/audio/transcriptions`. Le formulaire contient `file`, `model`, les
champs optionnels `language` et `prompt`, puis `response_format=json`. La réponse
attendue est un objet JSON contenant une chaîne `text` non vide.

Le modèle ne peut pas être vide ou blanc. `timeout_s` est borné de 1 à 86400 s
pour le STT comme pour le cleanup; la conversion en `time.Duration` intervient
seulement après ce préflight.

Le secret est résolu en amont, par exemple avec `config.SecretResolver`, puis
fourni à `stt.Config.APIKey`. L'en-tête `Authorization: Bearer ...` est absent
quand cette valeur est vide. Chaque instance possède son propre transport
`policy.TransportWithTrusted`; les proxies d'environnement sont désactivés et
les redirections ne sont pas suivies. Cela évite qu'un upload ou un credential
soit rejoué ou envoyé vers une destination différente.

Lorsqu'une source utilise `api_key_command`, le cœur exécute le helper avec
l'environnement parent moins chaque variable nommée par `api_key_env` dans les
tables STT et cleanup du snapshot complet. Principal, fallback, cleanup actif et
providers inactifs contribuent tous au filtre, y compris si plusieurs entrées
répètent le même nom. Les noms restent sensibles à la casse selon la sémantique
Unix. `PATH`, `HOME`, `XDG_RUNTIME_DIR`, les adresses D-Bus et les sockets ou
répertoires d'agents restent disponibles tant qu'ils ne sont pas eux-mêmes
déclarés comme `api_key_env`.

`Timeout` borne l'opération complète, retries et backoff compris. `Retries`
désigne le nombre d'essais supplémentaires, soit au plus `1 + Retries` appels.
La matrice de rejeu du `POST` STT est normative:

| Résultat d'un appel | Retry automatique |
|---|---|
| HTTP 401 ou 403 | non |
| HTTP 408 | oui |
| HTTP 429 | oui |
| HTTP 5xx | oui |
| autre statut HTTP | non |
| erreur de transport, aucun octet multipart lu par le transport local | oui |
| erreur de transport après au moins un octet multipart lu | non |
| réponse 2xx au corps illisible, trop grand, invalide ou vide | non |
| erreur de policy ou annulation | non |

Le compteur d'octets est un critère local prudent, pas une preuve de réception
distante. Une erreur de transport avant toute réponse ne permet pas toujours de
savoir ce que le serveur a reçu ou traité. Dès qu'une émission partielle est
détectée, Murmur ne rejoue pas l'appel sur cette seule erreur; si le serveur
renvoie toutefois un 408, 429 ou 5xx après avoir lu ou traité le corps, le retry
explicite peut dupliquer le travail distant. Le contrat borne seulement le nombre
d'appels; il ne fournit jamais de garantie exactly-once.

`health_enabled=false` désactive le health check, même si un endpoint est
présent. Avec `health_enabled=true`, `health_endpoint` est obligatoire. Une URL
absolue est acceptée seulement si elle conserve l'origine de `base_url`; un
chemin commençant par `/` est résolu depuis cette origine, et un chemin relatif
est ajouté à `base_url`. L'origine compare le schéma, l'hôte et le port effectif.
La règle HTTPS de `base_url` s'applique aussi à l'URL résolue: HTTP en zone
`internet` exige `allow_insecure=true`. Il n'existe aucun opt-in cross-origin et
le bearer n'est jamais envoyé à une autre origine. Ces contrôles ont lieu avant
toute requête, y compris lorsque le health check est désactivé mais qu'un endpoint
est présent.

Le cœur et `doctor` valident cette règle puis construisent le même
`stt.OpenAICompatible` et appellent son contrat `Health`: transport, refus des
redirections, timeout et authentification sont donc identiques. Aucun endpoint
`/models` n'est inventé.

Les champs TOML `retries` et `retry_backoff_ms` alimentent directement `Retries`
et `RetryBackoff`. Les valeurs sont bornées à 10 essais supplémentaires et 60 s
de backoff; le timeout global continue d'inclure tous les essais et toutes les
attentes entre eux.

### Fallback et segments

`stt.Chain` réutilise le même `stt.Request.Audio` (`wav.File`) pour le provider principal
et le fallback. Pour plusieurs segments, les index sont triés et les résultats
sont assemblés dans cet ordre avec une jointure à un espace.

Par défaut, un échec du provider principal relance tous les segments avec le
fallback et écarte les résultats primaires: une transcription ne mélange donc
pas silencieusement les providers. `AllowMixedProviders` autorise explicitement
un fallback par segment; `MultiResult.Segments` trace alors le provider effectif
de chaque segment et `WarningMixedProviders` est émis si plusieurs providers ont
été utilisés. Dans ce dernier cas, `MultiResult.Provider` vaut toujours `mixed`,
indépendamment de l'ordre des segments; un résultat mono-provider conserve son
nom habituel.

Un échec de segment ne livre aucun texte par défaut. `AllowPartial` omet
explicitement les segments en échec, marque `MultiResult.Partial` et ajoute
`WarningPartial`. Ce mode est une récupération dégradée, pas une réussite
silencieuse. Le warning conserve la classe et le provider de la première
tentative finalement omise, avec le même contrat en batch et en streaming.

Chaque appel public de `stt.Chain` crée un budget unique pour tous ses segments
et toutes ses tentatives provider. Il compte les octets UTF-8 des textes réussis
et leurs séparateurs d'assemblage. Un fallback complet ne remet pas le compteur
à zéro: les résultats primaires réussis mais finalement écartés restent donc
comptés. Le dépassement est toujours `server_error: volume cumulé des
transcriptions supérieur à la limite configurée`; aucun `MultiResult` partiel
n'est retourné, y compris lorsque `AllowPartial` est actif.

Le contexte interne du pool est annulé dès le dépassement. Les requêtes HTTP en
vol sont interrompues et aucun nouveau job n'est accepté. `TranscribeStream` ne
prend pas possession du canal et ne détache aucun drain: après son retour, le
producteur doit arrêter ses envois et fermer sa source. En session live,
l'orchestrateur annule explicitement le forwarder, abort la capture puis attend sa
terminaison. Le pool est récolté avant le retour. Sa concurrence effective est aussi limitée
à `ceil(MaxTextBytes / MaxResponseBytes)`, avec `MaxResponseBytes = 4 Mio`, afin
de borner raisonnablement les corps simultanément matérialisés avant leur ajout
au budget.

Toute erreur de budget conserve le provider de la tentative qui a franchi la
limite. Cette provenance alimente les résultats de session et CLI même si aucun
texte partiel n'est livré.

`MaxSegments` borne séparément les requêtes et résultats conservés pour le tri et
le fallback. Les canaux bornés ne remplacent pas cette limite cumulative. Le
dépassement est `server_error: nombre de segments STT supérieur à la limite
configurée` et suit la même règle de résultat vide.

Un fallback réussi ajoute un warning structuré avec le provider principal et la
classe de son échec. Si principal et fallback échouent, l'erreur synthétique
conserve les deux causes pour `errors.Is/As`, expose le dernier provider tenté et
n'affiche que les noms et classes, jamais les messages bruts susceptibles de
contenir un secret.

## Cleanup OpenAI-compatible

`internal/cleanup.Client` envoie un `POST` JSON vers
`{base_url}/chat/completions`, avec un message système et le texte protégé comme
message utilisateur. L'instruction vient de `cleanup.prompt`; un prompt strict
de correction légère est fourni lorsqu'elle est vide. Les entrées
`cleanup.providers.*` ont un schéma séparé et refusent les champs STT
`language`, `prompt`, `health_*`, `retries` et `retry_backoff_ms`. Le transport
applique les mêmes garanties de
policy, d'absence de proxy, de timeout, d'authentification conditionnelle et de
refus des redirections que le STT.

La sortie est rejetée si elle est vide, commence par une formulation méta, un
refus ou un commentaire connu, ou si les tokens sentinelles ne sont pas
conservés exactement dans leur ordre. Le validateur reconnaît le format produit
par le pipeline, `[[MURMUR_CMD:<nonce>:<index>:<intention>]]`, sans dépendre de
`internal/textpipe`, ce qui évite un cycle entre le pipeline et le client. Le
format historique `__[A-Z][A-Z0-9_]*__` reste également contrôlé.

`Client.Clean` ne détruit jamais l'entrée. Sur erreur HTTP, timeout, annulation
ou rejet d'un garde-fou, il retourne le texte d'entrée, l'avertissement
`cleanup.WarningDegraded` et la cause classifiée dans `Result.Degraded`.
`core.BuildProcessor` transmet cette erreur typée au pipeline, qui en conserve la
classe dans ses warnings, les événements, les sorties CLI et le dernier résultat
sans sérialiser la cause ni le corps HTTP.

## Classification D17

| Situation | Classe |
|---|---|
| HTTP 401 ou 403 | `auth_failed` |
| HTTP 408 ou deadline | `timeout` |
| HTTP 429 ou erreur réseau | `unavailable` |
| HTTP 5xx, autre statut ou JSON invalide | `server_error` |
| texte absent ou garde-fou cleanup | `empty_response` |
| annulation du contexte | `cancelled` |
| destination refusée par le transport | `policy_blocked` |

Pour une réponse non-2xx, le statut détermine la classe avant toute lecture du
corps. Celui-ci est drainé dans une limite de 4 Mio puis fermé; son contenu et
ses éventuelles erreurs de lecture ne sont repris ni dans l'erreur ni dans les
warnings. Pour une réponse 2xx, la limite de 4 Mio et les erreurs de lecture ou
de décodage restent classées `server_error`.
