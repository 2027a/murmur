# Pipeline de texte

`internal/textpipe` applique les primitives vocales déterministes après
assemblage du transcript complet:

```text
transcript assemblé -> protect -> cleanup? -> render -> plugin?
```

Le cleanup reçoit donc le contexte global et n'est appelé qu'une fois. Les
étages optionnels sont injectés via `textpipe.Cleanup` et `textpipe.Plugin`; le
package n'importe aucun client réseau ni plugin concret. Une erreur d'étage
optionnel conserve la meilleure version locale et ajoute un warning au résultat.
Ce warning contient une `class` issue de `internal/errs`, un `component` fermé
(`cleanup` ou `plugin`) et un message contrôlé. La cause, son texte et le
composant éventuellement fourni par une implémentation ne sont jamais recopiés:
un timeout, un refus de policy, un échec d'authentification ou une indisponibilité
restent distinguables sans exposer le transcript ni un détail provider.

## Packs

Les packs JSON `fr` et `en` sont embarqués dans le binaire. Ils mappent leurs
variantes, insensibles à la casse, vers quatre intentions communes:
`open_quote`, `close_quote`, `line_break` et `paragraph_break`.

`LoadPack("auto", sttLanguage)` suit le tag de langue STT. Les sous-tags tels
que `fr-FR` et `en_US` sont ramenés à leur langue de base. La validation de
configuration exige alors une langue principale explicite `fr` ou `en` et la
même langue de base pour le fallback. Une détection STT automatique vide ne peut
donc plus échouer tardivement pendant la construction du pipeline.

Le rendu par défaut utilise `« texte »` en français et `“text”` en anglais. Le
réglage `general.ascii_quotes=true` active le rendu ASCII `" texte "`. Il est
transmis par `core.BuildProcessor`, donc identique pour les sessions et les
commandes composables. Les ponctuations
parasites `.,;:!?` accolées aux commandes de ligne/paragraphe sont supprimées;
les virgules et points accolés à une commande de guillemet sont normalisés.
Après une fermeture de guillemets, la prose et une nouvelle citation sont
séparées par un espace; une ponctuation à accoler ou un saut de ligne n'ajoute
pas d'espace. Cette règle vaut pour les deux packs et le mode ASCII, même si le
cleanup a supprimé le séparateur devant la prose. Un texte sans commande reste
strictement inchangé en mode plain.

## Protection du cleanup

Chaque appel à `Protect` génère un namespace aléatoire de 128 bits. Le namespace
est accepté seulement s'il est absent de l'entrée; une collision provoque une
nouvelle génération, jamais un échappement ambigu. Les tokens incluent ensuite
le namespace, leur index et leur intention.

`Protection` est transmis au cleanup avec le texte protégé. Sa méthode
`Validate` exige les mêmes tokens, une seule fois chacun, dans le même ordre et
avec leur valeur exacte. Le pipeline refait cette validation avant le rendu:
suppression, duplication, permutation ou altération entraîne le rejet du cleanup
et un warning `empty_response`. Le renderer consomme tous les tokens générés
avant le plugin.

Exemple d'injection:

```go
pipe, err := textpipe.New(textpipe.Options{
	CommandPack: "auto",
	STTLanguage: "fr",
	Cleanup: textpipe.CleanupFunc(func(ctx context.Context, protected string, commands textpipe.Protection) (string, error) {
		cleaned, err := client.Cleanup(ctx, protected)
		if err != nil {
			return "", err
		}
		if err := commands.Validate(cleaned); err != nil {
			return "", err
		}
		return cleaned, nil
	}),
})
```

Le plugin concret, son timeout et sa zone de confiance sont décrits dans
[`markdown.md`](markdown.md). `textpipe` ne connaît que l'opération runtime texte
vers texte et traite toute erreur comme récupérable. En amont, le runtime
`internal/plugin` accepte une implémentation `Local` simple, mais exige le
contrat `External` à capability HTTP netguard pour toute zone `lan` ou
`internet`; une déclaration de zone seule ne donne aucun transport.

Les tests normatifs sont dans `internal/textpipe/testdata/golden.json`. Ils
reprennent les cas utiles du pipeline historique et ajoutent le scénario anglais
18, les collisions de namespace, les altérations de tokens, la parité des
intentions et l'indépendance des packs.
