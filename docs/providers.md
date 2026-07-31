# Providers

## STT OpenAI-compatible

`internal/stt.OpenAICompatible` envoie un `POST multipart/form-data` vers
`{base_url}/audio/transcriptions`. Le formulaire contient `file`, `model`, les
champs optionnels `language` et `prompt`, puis `response_format=json`. La réponse
attendue est un objet JSON contenant une chaîne `text` non vide.

Le secret est résolu en amont, par exemple avec `config.SecretResolver`, puis
fourni à `stt.Config.APIKey`. L'en-tête `Authorization: Bearer ...` est absent
quand cette valeur est vide. Chaque instance possède son propre transport
`policy.TransportWithTrusted`; les proxies d'environnement sont désactivés et
les redirections ne sont pas suivies. Cela évite qu'un upload ou un credential
soit rejoué ou envoyé vers une destination différente.

`Timeout` borne l'opération complète, retries et backoff compris. `Retries`
désigne le nombre d'essais supplémentaires. Un retry est possible après une
réponse 5xx explicite. Pour un échec de transport, il n'est possible que si le
transport n'a lu aucun octet du corps multipart. Un POST parti sans réponse
claire n'est donc jamais rejoué. Les erreurs d'authentification, de policy, de
contenu et d'annulation ne sont pas retentées.

`health_enabled=false` désactive le health check, même si un endpoint est
présent. Avec `health_enabled=true`, `health_endpoint` est obligatoire. Une URL
absolue est utilisée telle quelle, un chemin commençant par `/` est résolu depuis
l'origine de `base_url`, et un chemin relatif est ajouté à `base_url`. Le cœur et
`doctor` construisent le même `stt.OpenAICompatible` et appellent son contrat
`Health`: transport, refus des redirections, timeout et authentification sont
donc identiques. Aucun endpoint `/models` n'est inventé.

Les champs TOML `retries` et `retry_backoff_ms` alimentent directement `Retries`
et `RetryBackoff`. Les valeurs sont bornées à 10 essais supplémentaires et 60 s
de backoff; le timeout global continue d'inclure tous les essais.

### Fallback et segments

`stt.Chain` réutilise le même `stt.Request.Audio` (`wav.File`) pour le provider principal
et le fallback. Pour plusieurs segments, les index sont triés et les résultats
sont assemblés dans cet ordre avec une jointure à un espace.

Par défaut, un échec du provider principal relance tous les segments avec le
fallback et écarte les résultats primaires: une transcription ne mélange donc
pas silencieusement les providers. `AllowMixedProviders` autorise explicitement
un fallback par segment; `MultiResult.Segments` trace alors le provider effectif
de chaque segment et `WarningMixedProviders` est émis si plusieurs providers ont
été utilisés.

Un échec de segment ne livre aucun texte par défaut. `AllowPartial` omet
explicitement les segments en échec, marque `MultiResult.Partial` et ajoute
`WarningPartial`. Ce mode est une récupération dégradée, pas une réussite
silencieuse.

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
