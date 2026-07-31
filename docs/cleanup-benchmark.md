# Benchmark du cleanup OpenRouter

## Protocole

Benchmark exécuté le 31 juillet 2026 avec le même prompt et les mêmes textes
synthétiques pour les deux modèles. Les cas couvrent accords, ponctuation,
énumération, termes techniques et conservation d'une sentinelle Murmur.

Modèles testés:

- `openai/gpt-4o-mini`;
- `openai/gpt-5.6-luna`.

Les coûts et nombres de tokens viennent directement du champ `usage` des
réponses non streamées OpenRouter. Les latences mesurent les requêtes HTTP
complètes depuis le poste Murmur.

## Résultats

| Modèle | Coût total, 2 cas | Latence totale | Raisonnement invisible | Résultat |
|---|---:|---:|---:|---|
| GPT-4o mini | `$0.00013020` | `3.56 s` | `0` token | corrections justes, style et termes techniques préservés |
| GPT-5.6 Luna | `$0.00028140` | `7.25 s` | `265` tokens | corrections justes, mais réécriture stylistique et capitalisation de `Nginx` |
| GPT-5.6 Luna `low` | `$0.00011880` | `2.75 s` | `0` token | qualité proche, mais nombre grammatical et casse de `nginx` modifiés dans un cas |
| GPT-5.6 Luna `none` | `$0.00011940` | `1.94 s` | `0` token | même tendance que `low`, plus rapide |

Luna est environ 2,16 fois plus cher et 2 fois plus lent sur ce corpus. Son
tarif d'entrée catalogue est inférieur, mais ses tokens de raisonnement rendent
le coût réel supérieur pour cette tâche courte.

Avec `reasoning.effort=low` ou `none`, Luna n'a consommé aucun token de
raisonnement sur ce corpus. Il devient alors environ 8 % moins cher que GPT-4o
mini; `none` est aussi environ 45 % plus rapide. Ce gain s'accompagne toutefois
d'une fidélité légèrement inférieure: `les sauvegardes automatiques` devient
`la sauvegarde automatique` et `nginx` devient `Nginx` dans le premier cas.

## Choix

`openai/gpt-4o-mini` reste retenu pour la production: Luna sans raisonnement est
un peu moins cher et plus rapide, mais GPT-4o mini respecte mieux le contrat de
correction légère sans changement de nombre, de style ou de casse technique.
Luna `none` reste un candidat si la priorité devient la latence plutôt que la
fidélité stricte.

## Autres Candidats OpenRouter

Un second panel a été testé sur les mêmes deux cas. Les modes de raisonnement
ont été désactivés lorsque l'API le permettait; GPT-5 Nano exige au minimum
`reasoning.effort=minimal`.

| Modèle | Coût total, 2 cas | Latence réussie | Fidélité | Limite observée |
|---|---:|---:|---|---|
| GPT-4o mini | `$0.00013020` | `3.56 s` | meilleure référence | aucune sur le test |
| GPT-5 Nano `minimal` | `$0.00008390` | `4.51 s` | très proche de GPT-4o mini | latence variable; raisonnement obligatoire |
| Gemini 2.5 Flash Lite | `$0.00008570` | `3.72 s` | bonne | formalise `qu'on a` en `que nous avons` |
| Mistral Small 3.2 24B | `$0.00007480` | `2.45 s` | bonne | réécrit `parce que` en `car` et capitalise `Nginx` |
| Qwen 3.7 Flash | `$0.00002785` | `2.32 s` | correcte | un `429` du pool partagé au deuxième appel; réécriture stylistique au retry |

GPT-5 Nano est le seul remplaçant évident si le coût devient prioritaire tout en
conservant une fidélité proche. Il exige toutefois que Murmur expose le paramètre
de raisonnement `minimal`; sans ce paramètre, le comportement et le coût par
défaut ne sont pas ceux mesurés ici. Mistral est le meilleur compromis de
latence parmi les modèles sans raisonnement, mais respecte moins strictement le
style. Qwen est de loin le moins cher, au prix d'une disponibilité insuffisante
sur le pool OpenRouter partagé observé pendant le test.

Sources:

- catalogue et tarifs: <https://openrouter.ai/api/v1/models>;
- schéma API, usage et coût: <https://openrouter.ai/docs/api/reference/overview>.
