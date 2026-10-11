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
credentials utilise un exécuteur séparé et borné soumis au même filtre, sur tous
les providers du snapshot et pas seulement ceux que `doctor` vérifie.

Chaque check externe effectivement exécuté possède son propre budget de cinq
secondes: résolution d'un credential, `pw-cli info`, health provider, ping D-Bus
ou `<helper OSD> --check`. Pour un `api_key_command`, ce plafond de `doctor` à
cinq secondes est plus court que le timeout interne du resolver et constitue
donc le timeout effectif. Une expiration annule le check. Pour les commandes
exécutées via `sysexec`, tout le groupe de processus est alors tué et le processus
lancé est récolté. L'expiration produit un check `failed`, puis `doctor` continue
les checks indépendants. Le contexte transmis à `doctor.Run` reste prioritaire:
son annulation interrompt chaque check en cours et tous les checks suivants
reçoivent immédiatement cette annulation.

Les checks étant séquentiels, leur budget cumulé vaut `5 s × N`, où `N` est le
nombre de checks bornés réellement exécutés. Il dépend donc de la configuration
et des échecs précédents: un credential en échec saute le health correspondant.
Une configuration exécutant une source explicite, trois résolutions de
credentials (STT principal, fallback et cleanup), deux health STT et une UI
D-Bus ou OSD cumule au pire 35 s de budgets externes. Il n'existe pas de plafond
global fixe. Un contexte global plus court réduit cette somme; les accès
locaux au système de fichiers et les recherches d'exécutables ne constituent pas
une promesse de deadline globale.

Le premier check applique exactement `config.Validate`. En cas d'échec, le
rapport ne contient que `configuration et policy` avec ce même message, puis
s'arrête avant runtime, recherche d'outil, source PipeWire, helper, UI ou réseau.
Cela couvre notamment modèles vides, raccourcis inconnus, combinaisons clipboard
non persistantes et dépassements des bornes documentées.

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
| collage effectif avec raccourci et `paste_backend=wtype` | `wtype` |
| collage effectif avec raccourci et `paste_backend=hyprland` | `hyprctl`, session et syntaxe via `hyprctl -j status`, sans envoyer de touches |
| frappe effective (`type`, raccourcis vides ou clipboard `none`) | `wtype`; aucune interrogation de Hyprland |
| `sink.mode=clipboard` | `wl-copy` |
| `sink.mode=file` | fichier existant inscriptible ou parent permettant sa creation |
| `ui.mode=notify` | bus de session et `org.freedesktop.Notifications` joignables |
| `ui.mode=auto` | acquittement du module QuickShell; à défaut, vérification des notifications |
| `ui.mode=quickshell` | acquittement du module QuickShell obligatoire, sans repli |
| `ui.mode=osd` | helper `ui.osd.helper` présent et `<helper> --check` réussi |
| `ui.sound=true` | au moins un de `pw-play`, `paplay`, `canberra-gtk-play` |

Une configuration `sink.mode="stdout"`, `sink.clipboard_mode="none"` et
`ui.mode="none"` ne verifie donc ni `wl-copy`, ni `wtype`, ni `hyprctl`, ni D-Bus, ni OSD.
`auto` est interprété selon `sink.auto_mode` pour ces checks.
Un provider present dans une table mais non reference comme principal,
fallback ou cleanup actif est ignore, credential compris. C'est le scenario
d'acceptation 14.

## Sante provider

`health_enabled=false` ajoute un check `skipped` et n'effectue aucune requête.
Lorsque le réglage vaut `true`, `health_endpoint` est obligatoire et `doctor`
utilise le même client et le même contrat `Health` que le cœur. Le timeout du
diagnostic est plafonné à cinq secondes, les redirections restent refusées et le
credential reste limité à la requête initiale. Chaque résolution de credential
reçoit le même plafond local de cinq secondes; un délai interne effectivement
plus court et l'annulation globale restent prioritaires. L'endpoint est résolu et
validé avant la résolution du credential: une URL absolue doit garder l'origine
de `base_url`, et la même règle HTTPS s'applique en zone `internet`. Un endpoint
depuis l'origine (`/health`) ou relatif à `base_url` suit les règles documentées
dans [`providers.md`](providers.md). `doctor` ne dérive jamais `/models`.

## Limites du schéma

Le schéma ne possède pas de booléen séparé « authentification requise »:
`doctor` résout un credential uniquement lorsqu'une source `api_key_*` est
configurée. Le helper, la position et les délais OSD sont configurables; le
fichier son reste le son freedesktop
`/usr/share/sounds/freedesktop/stereo/bell.oga`.

Tous les effets externes sont injectables via `doctor.Dependencies`: recherche
d'executables, runtime, fichier, source PipeWire, providers, secrets, D-Bus et
`sysexec.Executor`. Les tests n'ont donc besoin d'aucun desktop reel.
