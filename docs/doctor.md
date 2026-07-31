# Doctor

## Contrat

`doctor.Run(ctx, cfg, dependencies)` construit ses checks uniquement depuis la
`config.Config` deja resolue. Il ne relit ni TOML ni variable de surcharge. Un
rapport v1 peut etre rendu par `WriteHuman` ou `WriteJSON`.

```json
{"v":1,"ok":true,"checks":[{"name":"configuration et policy","status":"ok"}]}
```

Les statuts sont `ok`, `failed` et `skipped`. Une erreur de credential marque le
check de sante correspondant `skipped`, sans tenter une requete vouee a l'echec.
Les messages ne contiennent ni secret ni corps de reponse provider.

Les commandes de diagnostic (`pw-cli`, helper OSD) reçoivent un environnement
filtré sans les variables déclarées par `api_key_env`. La résolution des
credentials utilise un exécuteur séparé et borné.

## Matrice conditionnelle

Toujours verifies:

- validation de la configuration et de la policy effectives;
- repertoire runtime prive, mode `0700`, accessible en ecriture;
- presence de `pw-record`;
- aucune capture de test; si une source explicite est configuree, verification
  non intrusive via `pw-cli info`; la source PipeWire par defaut est laissee au
  routage effectif de PipeWire;
- providers STT principal et fallback effectivement configurés; leur santé est
  testée seulement si `health_enabled=true`.

Checks ajoutes selon la configuration:

| Condition effective | Checks |
|---|---|
| provider avec un `api_key_*` | resolution du credential, sans l'afficher |
| `cleanup.enabled=true` | credential éventuel; health ignoré car le schéma cleanup n'en expose pas |
| `sink.clipboard_mode != "none"` | `wl-copy` |
| `sink.mode=paste` ou `auto` | `wl-copy`, `wtype` |
| `sink.mode=type` | `wtype` |
| `sink.mode=clipboard` | `wl-copy` |
| `sink.mode=file` | fichier existant inscriptible ou parent permettant sa creation |
| `ui.mode=notify` | bus de session et `org.freedesktop.Notifications` joignables |
| `ui.mode=osd` | helper `ui.osd.helper` présent et `<helper> --check` réussi |
| `ui.sound=true` | au moins un de `pw-play`, `paplay`, `canberra-gtk-play` |

Une configuration `sink.mode="stdout"`, `sink.clipboard_mode="none"` et
`ui.mode="none"` ne verifie donc ni `wl-copy`, ni `wtype`, ni D-Bus, ni OSD.
Un provider present dans une table mais non reference comme principal,
fallback ou cleanup actif est ignore, credential compris. C'est le scenario
d'acceptation 14.

## Sante provider

`health_enabled=false` ajoute un check `skipped` et n'effectue aucune requête.
Lorsque le réglage vaut `true`, `health_endpoint` est obligatoire et `doctor`
utilise le même client et le même contrat `Health` que le cœur. Le timeout du
diagnostic est plafonné à cinq secondes, les redirections restent refusées et le
credential reste limité à la requête initiale. Un endpoint absolu, depuis
l'origine (`/health`) ou relatif à `base_url` suit les règles documentées dans
[`providers.md`](providers.md). `doctor` ne dérive jamais `/models`.

## Limites du schéma

Le schéma ne possède pas de booléen séparé « authentification requise »:
`doctor` résout un credential uniquement lorsqu'une source `api_key_*` est
configurée. Le helper, la position et les délais OSD sont configurables; le
fichier son reste le son freedesktop
`/usr/share/sounds/freedesktop/stereo/bell.oga`.

Tous les effets externes sont injectables via `doctor.Dependencies`: recherche
d'executables, runtime, fichier, source PipeWire, providers, secrets, D-Bus et
`sysexec.Executor`. Les tests n'ont donc besoin d'aucun desktop reel.
