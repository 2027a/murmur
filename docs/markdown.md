# Plugin Markdown

Le plugin Markdown initial est local, déterministe, sans appel réseau et
désactivé par défaut. Le flux complet est:

```text
transcript -> protect -> cleanup? -> render -> plugin Markdown? -> sink
```

Activation explicite:

```toml
[plugin]
enabled = true
mode = "markdown"
timeout_s = 2

[policy]
plugin_zones = ["local"]
```

`mode = "plain"` est l'identité. `enabled = false` omet entièrement l'étape,
quel que soit le mode configuré. Le runtime impose le timeout et vérifie la zone
déclarée par l'implémentation.

Le contrat distingue `plugin.Local` et `plugin.External`. Un plugin local garde
la signature simple `Transform(ctx, text)`. Un plugin déclarant `lan` ou
`internet` est refusé s'il expose seulement cette signature: il doit implémenter
`Transform(ctx, capability, text)`. Le runtime construit la capability HTTP avec
`policy.TransportWithTrusted`, pour la zone déclarée et les CIDR de
`policy.trusted_networks`; le plugin ne reçoit ni le client privé ni son
transport remplaçable. `Capability.Do` remplace aussi le contexte de la requête
par le contexte borné du runtime. Cette interface borne le chemin HTTP
coopératif. Un plugin Go chargé dans le même processus n'est pas une sandbox
contre du code hostile; un tel code doit être isolé dans un processus distinct.

```go
type ExternalMarkdown struct {
	Endpoint string
}

func (ExternalMarkdown) Name() string      { return "external-markdown" }
func (ExternalMarkdown) Zone() policy.Zone { return policy.LAN }
func (p ExternalMarkdown) Transform(ctx context.Context, net plugin.Capability, text string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint, strings.NewReader(text))
	if err != nil {
		return "", err
	}
	resp, err := net.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return "", fmt.Errorf("status HTTP inattendu: %s", resp.Status)
	}
	return text, nil
}
```

## Grammaire initiale

Une transformation exige le déclencheur explicite `commande stt`. La variante
ASR plurielle `commandes stt`, la casse et une ponctuation intercalaire sont
acceptées. Sans ce déclencheur, les mots `titre`, `liste`, `gras` et `italique`
restent du texte ordinaire.

Formes prises en charge:

```text
commande stt titre 1 journal de bord fin du titre
commande stt titre de niveau deux architecture fin du titre
commande stt gras texte fin du gras
commande stt italique texte fin de l'italique
commande stt liste à puces item alpha item deux beta fin de liste
commande stt liste numérotée item un alpha item deux beta fin de liste
```

Les variantes `liste a puce(s)`, `liste numero T`, `item numéro 1` et une
répétition de `commande stt` avant une fermeture sont issues des fixtures ASR
historiques. Citations, code, liens, tableaux, checklists, imbrications et état
persistant sont hors périmètre.

Chaque structure possède une fermeture lexicale: `fin du titre`, `fin du gras`,
`fin de l'italique` ou `fin de liste`. La ponctuation n'est jamais une borne de
contenu: `Dr. Martin: état des lieux?` reste donc entier dans un titre jusqu'à
`fin du titre`. Le point, point d'interrogation ou point d'exclamation placé
immédiatement après la fermeture d'un bloc titre/liste est traité comme la
ponctuation ASR de cette fermeture; le texte libre qui suit reste inchangé.

Une liste ordonnée peut employer uniquement des marqueurs nus `item`, ou une
suite entièrement numérotée. Dès qu'un marqueur porte un numéro, tous doivent en
porter un et suivre exactement `1, 2, 3...`; le parseur refuse un départ à 2, un
trou, une répétition, un mélange ou un `item numéro` invalide au lieu de masquer
l'erreur en renumérotant. Dans une liste à puces, `item un problème` conserve
`un problème` comme contenu.

## Validation et repli

Le parseur produit uniquement des segments `text`, `heading` de niveau 1 ou 2,
`bold`, `italic`, `ordered_list` et `bullet_list`. Le document entier est validé
avant rendu: structure explicitement fermée, contenu non vide, absence de
commande vocale non consommée et absence de token technique. Une commande
inconnue, non bornée ou une structure invalide fait échouer le plugin.

Cette erreur est récupérable dans `textpipe`: le texte post-cleanup/post-render
est livré sans réécriture partielle et un warning `plugin` est ajouté. Les
fixtures normatives sont dans
`internal/plugin/markdown/testdata/fixtures.json`; elles reprennent les formes
observées sur la branche historique `feat/markdown-v0-fixtures`, les ambiguïtés
de ponctuation et les entrées adversariales. Les fixtures invalides sont aussi
exécutées à travers `textpipe` pour vérifier le repli post-render et l'absence de
tokens `MURMUR_CMD`/`__WQ_` dans le résultat livré.
