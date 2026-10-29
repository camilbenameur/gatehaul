# Spécification technique de `gatehaul` v0.1

Ce document décrit ce que fait `gatehaul` et comment : l'interface, le protocole entre le laptop et l'hôte, le runner, les états des jobs et l'assainissement des patchs. Le code renvoie à ses sections (`docs/SPEC.md §5.2`, par exemple).

## 1. Objet

Dans n'importe quel dépôt GitHub, une fois `gh` connecté, `gatehaul run -- go test ./...` marche sans fichier de configuration. Les jobs tournent dans les Codespaces de l'utilisateur, survivent à la session de l'agent qui les a lancés, et ne passent pas au vert sans preuve d'exécution.

Le backend de la v0.1 est GitHub Codespaces, piloté par `gh`. Les interfaces `Transport` et `Lifecycle` (§6) permettront d'ajouter d'autres hôtes (SSH, Docker) sans refonte.

## 2. Ce qui est deviné, et comment

| Information | Source | Repli quand elle manque |
|---|---|---|
| Binaire `gh` et sa version | `gh --version` (minimum 2.40) | `doctor` explique comment installer ou mettre à jour |
| Compte et authentification | `gh auth status --active --json hosts` | `doctor` → `gh auth login` |
| Scope `codespace` | Champ des scopes de `gh auth status` | `doctor` → `gh auth refresh -h github.com -s codespace` |
| Dépôt `owner/repo` | `git remote get-url origin` (puis `upstream`, puis le seul remote GitHub). L'URL est analysée, qu'elle soit en SSH ou en HTTPS. | `--repo owner/repo` |
| Ce qu'on envoie | Le **commit** de `HEAD`, ou la référence passée en argument. Un working tree sale déclenche un avertissement, jamais un envoi. | Erreur claire : « commitez d'abord » |
| Machines utilisables | `gh codespace list --repo R --json name,displayName,state,machineName,repository,lastUsedAt`, filtré sur le **préfixe d'affichage `gatehaul-`** | `gatehaul up` (§4) |
| Type de machine par défaut | `gh api repos/R/codespaces/machines` → la plus petite machine autorisée | `--machine` |
| Ce que l'hôte connaît déjà | La sonde (§5.1) renvoie les références `refs/gatehaul/*` et la version du runner. Le contrôleur en déduit la base du bundle incrémental. | Bundle complet |
| Gate *(prévu)* | `gatehaul.toml` s'il existe. Sinon, détection : `go.mod` → `go test -json ./...` ; `package.json` avec un script `test` → `npm test` ; `Cargo.toml` → `cargo test` ; `pyproject.toml` → `pytest --junitxml`. | `gatehaul gate -- <cmd>` avec le parseur regex |
| Seuil minimal de tests *(prévu)* | **Cliquet** : le plus grand nombre de tests déjà observé sur la branche ou sa base, et jamais 0. Il est stocké **sur le laptop**, dans la ref `refs/gatehaul/ratchet/<clé>`, mise à jour par compare-and-swap (`git update-ref <ref> <nouveau> <ancien>`). | `min_tests` dans `gatehaul.toml` |
| Politique de gate *(prévu)* | Empreinte de la politique effective (étapes, commandes, parseurs, seuils, sauts autorisés), approuvée au premier usage et stockée dans le cache local | Toute modification ultérieure exige `--accept-drop` (code 4 sinon) |

**Ce qui n'est jamais deviné**, parce que la décision revient à l'humain : créer un codespace (qui consomme du quota), relever un seuil vers le bas ou changer la politique de gate, et supprimer quoi que ce soit. `gatehaul.toml` vit dans un dépôt que l'agent modifie : l'agent peut en écrire la première version, mais jamais approuver seul un assouplissement de sa propre gate.

## 3. Interface

| Commande | Effet | Codes de sortie | État |
|---|---|---|---|
| `gatehaul doctor [--json]` | Vérifie `gh`, l'authentification, le scope, git ≥ 2.31, le dépôt, le commit, la gate et la présence de codespaces `gatehaul-*`. Pour chaque problème, renvoie **la commande qui le corrige**. | 0 ok · 5 à corriger | fait |
| `gatehaul up [-n 1] [--machine M] [--max 2] [--yes] [--dry-run]` | Crée les codespaces `gatehaul-<i>` manquants, avec `--idle-timeout`, `--retention-period` et `--default-permissions` explicites, et affiche le coût. Demande confirmation dans un terminal ; **sans terminal et sans `--yes`, échoue avec une ligne `Fix:`** au lieu d'attendre une réponse qu'un agent ne donnera jamais. | 0 · 5 | fait |
| `gatehaul down [--delete]` | Arrête (ou supprime) les codespaces `gatehaul-*` dont la file est vide | 0 | fait |
| `gatehaul run [<ref>] [--prio P] [--timeout D] [--block 5m] -- <cmd>` | Envoie le commit, met en file et exécute. Bloque au plus `--block`, puis rend la main avec l'identifiant. | 0 vert · 1 rouge · 3 en cours · 5 infra | fait |
| `gatehaul gen [<ref>] [--allow-sensitive] -- <cmd>` | Comme `run`, puis applique au working tree local le patch des fichiers modifiés, après assainissement (§7) | idem, plus 6 patch refusé | fait |
| `gatehaul apply [<id>] [--allow-sensitive]` | Rejoue l'application du patch d'un `gen` terminé ; n'applique jamais deux fois | 0 · 3 · 5 · 6 | fait |
| `gatehaul wait [<id>...] [--any\|--all] [--block 5m]` | Idempotent, n'annule jamais. Rend `completed_ids` et `pending_ids`. Sans id, vise l'unique job non terminé de `HEAD` ; un préfixe unique de l'ULID suffit. Applique le patch d'un `gen` qui vient de finir. | comme `run` | fait |
| `gatehaul log [<id>] [--grep RE] [--since OCTETS] [--tail N]` | Log complet ou filtré, avec un curseur en octets rendu à chaque appel et un plafond par réponse (100 Kio) pour protéger le contexte de l'agent | 0 | fait |
| `gatehaul status [<id>]` · `gatehaul queue` | État d'un job, ou des files de chaque hôte avec l'attente estimée | 0 | prévu |
| `gatehaul cancel [<id>]` | TERM au groupe de session, KILL après 10 s, puis balayage des processus marqués `GATEHAUL_JOB` (le runner le sait déjà) | 0 | prévu |
| `gatehaul gate [<ref>] [--profile light\|full] [--accept-drop]` | Lance la gate. **Le verdict est calculé sur le laptop.** | 0 · 1 · 3 · **4 faux vert** · 5 | prévu |
| `gatehaul hook claude-stop` | Hook `Stop` de Claude Code : sortie 2 si la dernière gate verte ne porte pas sur `HEAD` | 0 · 2 | prévu |

**Sorties :** `--json` sur toutes les commandes. Le texte par défaut est court et lisible par un agent : première ligne `job=<id> state=<s> exit=<n>`, puis la fin du log, puis une ligne `next: <commande>` quand il reste quelque chose à faire. Les données vont sur stdout, les erreurs sur stderr. **Toute erreur porte une ligne `Fix:`** et un `code` stable dans le JSON (`outcome_unknown`, `scope_missing`, `host_replaced`, `patch_refused`, `stale_base`…). Les logs de job sont précédés d'une ligne qui les marque comme donnée non fiable.

**États et codes de sortie sont deux choses distinctes.** États : `queued`, `running`, puis un état terminal jamais réécrit (`succeeded`, `failed`, `timed_out`, `cancelled`, `lost`, `infra_failed`), avec `failure_kind` (`user`, `infra`, `lost`, `gate`) et `stage`. Une seule table de vérité, dans `internal/job/state.go` ; un état inconnu est une erreur, jamais une valeur par défaut. Correspondance : `succeeded` 0 ; `failed`, `timed_out` et `cancelled` 1 ; `queued` et `running` 3 ; `lost` et `infra_failed` 5. Seuls `run`, `gen`, `wait` et `gate` rendent 3 ; `log` rend 0 dès qu'il a pu lire.

**Identifiant de job :** `<n° d'hôte>-<ULID>`, par exemple `2-01JB…`. Le numéro vient du nom d'affichage `gatehaul-2` : on peut router sans index local, et l'index ne sert que de cache.

**Soumission idempotente :** le contrôleur fabrique l'ULID et écrit `jobs/<id>/dispatch.json` dans son cache *avant* d'ouvrir la connexion. Un `submit` rejoué avec le même id et la même empreinte renvoie le job existant (`exists`) ; avec une autre empreinte, il répond `conflict`. Si la réponse se perd, la sortie est 5 avec `outcome_unknown` et `next: gatehaul wait <id>` : jamais un échec de job, jamais un rejeu aveugle.

## 4. Cycle de vie des Codespaces

- **Nommage :** le nom d'affichage `gatehaul-<i>` est l'étiquette. Le vrai nom (aléatoire) n'est jamais codé en dur : il est redécouvert à chaque appel, avec un cache de 60 s. Deux codespaces portant le même numéro sont une erreur avec `Fix:`.
- **Création** (`gatehaul up` seulement, jamais implicite) : `gh codespace create -R R -b <branche par défaut> -m M -d gatehaul-<i> --idle-timeout 30m --retention-period 72h --default-permissions`. Un job détaché sans session SSH ouverte meurt à l'idle-timeout (mesuré : arrêt 5 min après la dernière session avec `--idle-timeout 5m`), alors qu'une session ouverte, même silencieuse, garde l'hôte éveillé ; d'où 30 min par défaut et des `wait` par tranches.
- **Réveil :** si un hôte choisi est `Shutdown`, le premier `gh codespace ssh` le démarre (environ 20 s mesurées). Le contrôleur affiche `waking gatehaul-2…`.
- **Arrêt :** `gatehaul down`. Un hôte dont la file n'est pas vide est laissé en place, avec la raison.
- **Plafond :** par défaut, `up` refuse au-delà de 2 codespaces (conditions d'usage de Codespaces). `--max` permet de le relever.
- **Création concurrente :** `up` prend un verrou de fichier local pendant `list` puis `create`, et relit la liste après avoir obtenu le verrou : deux agents lancés en même temps ne dépassent pas le plafond. Tout hôte encore listé occupe son numéro, même en cours de suppression.
- **États :** ceux de `gh` (`Available`, `Starting`, `ShuttingDown`, `Provisioning`, `Shutdown`, `Failed`…) sont rangés dans `Running`, `Busy`, `Stopped`, `NotFound` ou `Failed`. Un état inconnu n'est jamais `Running` : il marque l'hôte concerné comme inutilisable, sans bloquer les autres. On respecte `GH_HOST` et on n'appelle jamais `gh auth switch`.
- **Remplacement :** `/workspaces/.gatehaul/host.id`, tiré au hasard à l'installation, est renvoyé par `probe` et recopié dans chaque job. Un codespace supprimé puis recréé sous le même nom donne `lost` avec `host_replaced`, pas « job inconnu ».

## 5. Côté hôte : le runner

Un seul script, `runner.sh`, est embarqué dans le binaire avec `go:embed`. Il est installé sous `/workspaces/.gatehaul/runner-<sha256 court>.sh`. `/workspaces` est persistant ; `/tmp` est effacé à chaque arrêt du codespace.

**Protocole `GATEHAUL1`.** La ligne de commande distante est toujours `bash -s` ; tout passe par stdin, dans cet ordre :

1. Un préambule, appelé d'une seule ligne pour que bash l'ait lu en entier avant d'exécuter quoi que ce soit. Il affiche `GATEHAUL1 HELLO <version>` (preuve que le script distant a démarré), lit exactement la longueur du runner avec `LC_ALL=C read -N`, l'installe s'il manque ou si son sha256 ne correspond pas (écriture dans un `.tmp` puis `mv`, un seul gagnant), vérifie que la racine n'est pas montée `noexec`, puis exécute le runner.
2. Le runner lui-même (quelques dizaines de Ko).
3. L'en-tête de la requête : une ligne `GATEHAUL1 <sous-commande>`, des lignes `clé=valeur`, puis une ligne vide. Les valeurs sont restreintes à `[A-Za-z0-9._:,/+=-]` ; le texte libre (commande, motif de recherche) est en base64 dans une clé en `_b64`. Le runner valide chaque valeur et n'en évalue jamais aucune.
4. La charge utile, pour `submit` seulement : exactement `bundle_len` octets de bundle git.

La réponse est `GATEHAUL1 BEGIN`, une ligne de JSON, puis `GATEHAUL1 END <rc>`. Le contrôleur ignore le bruit du shell distant avant `HELLO`. Un flux avec `HELLO` mais sans `END` a été coupé : pour un `submit`, l'issue est inconnue (§3). Une coupure avant `HELLO` peut être réessayée, puisque rien n'a été fait.

```
/workspaces/.gatehaul/           0700 ; le runner tourne en umask 077, la commande du job en umask 022
  runner-<hash>.sh
  host.id              identifiant aléatoire de l'hôte (§4)
  failed               présent seulement si l'installation a échoué
  history.jsonl        durées passées
  repo.git/            dépôt nu ; refs/gatehaul/<clé> = dernier commit reçu
  wt/<clé>/            un worktree par branche
  jobs/<id>/           bundle, cmd.sh, meta.json, log, started, lock, sid, manifest.json, patch, exit.json
  queue/<prio>-<seq>-<id>   tickets en attente
  lock                 verrou du tour d'exécution
```

**`<clé>`** remplace le nom de branche brut : un slug `[a-z0-9-]` de 32 caractères au plus, suivi de 6 hexadécimaux du sha256 de la ref complète (`a/b`, `A` et `é` n'entrent pas en collision). Le contrôleur la calcule, et le runner la revalide avant toute concaténation de chemin. Tous les fichiers d'état s'écrivent dans un `.tmp` puis par `mv`, portent un champ `schema: 1`, et `exit.json` est **écrit en dernier, une seule fois**.

### 5.1 Sous-commandes du runner

Chacune correspond à **un seul aller-retour SSH**, qui coûte environ 5 s avec `gh codespace ssh` (mesuré, à chaud).

| Sous-commande | Rôle |
|---|---|
| `probe` | Renvoie la version du runner, `host_id`, `boot_id`, la profondeur de la file, le job en cours et son âge, les `refs/gatehaul/*`, les outils présents, l'espace disque libre et la présence d'une variable de token. **Lance aussi la réconciliation** (§5.3). |
| `submit` | Reçoit l'en-tête et le bundle. Idempotent (§3). Vérifie le bundle (`git bundle verify`, `list-heads` contient le SHA attendu, `transfer.fsckObjects`, taille plafonnée à 2 Gio), le fetch, écrit le ticket, détache le job avec `setsid` et renvoie l'identifiant. Si un prérequis du bundle incrémental manque, il répond `need_full` et le contrôleur renvoie un bundle complet sous le même id. |
| `wait` | Attend au plus 60 s la fin d'un ou de plusieurs jobs, puis renvoie leur état et la fin de leur log. Ordre imposé : lire `exit.json`, vider le log jusqu'au bout, puis répondre. Le contrôleur enchaîne des tranches indépendantes ; aucune attente orpheline ne reste sur l'hôte. |
| `log` | Log complet, filtré (`grep -E`) ou à partir d'un curseur en octets, coupé à la dernière fin de ligne, avec `cursor` et `truncated` |
| `patch` | Le patch d'un `gen` qui a réussi, en base64, avec son sha256 recalculé et sa taille (8 Mio au plus) ; `no_patch` sinon |
| `cancel` | TERM au groupe de session, KILL après 10 s, balayage `GATEHAUL_JOB`, retrait du ticket. N'écrit `cancelled` que si `exit.json` n'existe pas encore. |
| `gc` | Supprime les jobs terminés depuis plus de N jours |

### 5.2 Déroulé d'un job (mode `__job`, détaché)

1. `git fetch` du bundle dans `repo.git`, puis mise à jour de `refs/gatehaul/<clé>`. Cette étape n'écrit que dans `repo.git`.
2. **Attente du tour** : boucle `flock` sur le verrou global, avec le ticket en tête de `queue/`, triée par (priorité, séquence). L'attente est bornée à 60 min.
3. **Sous le verrou seulement** : `git worktree prune`, remise à zéro du worktree (`reset --hard` et `clean -ffdx`, en gardant une courte liste de caches), puis `checkout --detach <sha>`. Un worktree que `rm` ne peut pas supprimer (fichiers d'un autre utilisateur) est mis de côté, et `gc` le purge.
4. **Vérification que HEAD vaut le SHA envoyé.** Sinon, `infra_failed` avec `stage=verify-sha`.
5. Écriture de `started` et de l'identité du job (`sid`, heure de démarrage lue dans `/proc/<pid>/stat`, `boot_id`), puis exécution de `setsid --wait timeout --signal=TERM --kill-after=10s <durée> bash -o pipefail cmd.sh`, sous umask 022, avec `GATEHAUL_JOB=<id>` dans l'environnement. **Les variables de token (`GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_CODESPACE_TOKEN`…), `GIT_ASKPASS`, `SSH_ASKPASS` et `SSH_AUTH_SOCK` sont retirées**, et le *credential helper* est neutralisé. Limite connue : un codespace garde aussi des tokens dans des fichiers lisibles sous `/workspaces/.codespaces/shared/`, que la commande peut lire ; n'y lancez que des projets de confiance. Le job tient `jobs/<id>/lock` pendant toute sa vie, mais ce descripteur est fermé pour la commande, pour qu'un démon lancé par le build n'hérite pas du verrou. Le délai d'exécution (`--timeout`) est distinct de l'attente du tour ; seul un dépassement réel de ce délai donne `timed_out`.
6. TERM au groupe de session, KILL après 10 s, puis balayage borné de `/proc/*/environ` pour tuer ce qui porte `GATEHAUL_JOB=<id>`, y compris les processus qui ont refait `setsid`. S'il en reste, la raison mentionne `cleanup_unresolved`.
7. Pour un `gen` : le patch est calculé **contre le SHA vérifié, jamais contre HEAD** (la commande a pu committer ou changer de branche), avec un index privé : `read-tree <sha>`, `add -A` (sans les fichiers non suivis qui existaient avant la commande, comme les caches conservés), puis `diff --cached --binary --full-index --no-ext-diff --no-textconv --no-renames <sha>`, avec `--git-dir` et `--work-tree` explicites. Le calcul est borné (120 s, 8 Mio) ; au-delà, le job finit `failed` à l'étape `patch` avec une raison claire. Son `patch_sha256` et sa taille vont dans `exit.json`. Le worktree est ensuite remis à zéro.
8. Écriture de `manifest.json` : SHA vérifié, durées, cœurs, `uname`, versions de la toolchain lues dans le shell du job, et `tree_dirty_after` (une gate qui modifie des fichiers suivis ne donne pas de vert). Ajout de la durée à `history.jsonl`.
9. **Écriture de `exit.json` en dernier** (`schema`, `state`, `exit_code`, `failure_kind`, `reason`, `stage`, `truncated`, heures de début et de fin), puis retrait du ticket. Le log est plafonné à 16 Mio pendant l'exécution : au-delà, la commande est arrêtée (`failed`, `truncated`), parce que couper le log après coup invaliderait les curseurs déjà rendus par `log --since`.

Tous les appels git du runner passent par un wrapper unique : `-c core.hooksPath=/dev/null`, `-c credential.helper=`, `-c gc.auto=0`, `-c protocol.allow=never` (sauf `file`), `init --bare --template=`, `GIT_TERMINAL_PROMPT=0`, `LC_ALL=C`, et `rev-parse --verify --end-of-options <rev>^{commit}`, en refusant toute référence qui commence par `-`.

### 5.3 Réconciliation (à chaque `probe`, `wait`, `log` et `cancel`)

- Le verrou `jobs/<id>/lock` est libre, `started` existe et `exit.json` n'existe pas : le job est `lost` (codespace redémarré, processus tué). On écrit son `exit.json` et on retire son ticket. **Un job dont `started` existe n'est jamais relancé.**
- Un ticket sans `started` dont le processus a disparu (autre `boot_id`, ou verrou libre après 60 s de grâce) est **relancé une fois**, à sa place dans la file. Perdu une seconde fois sans avoir démarré, il finit `infra_failed` à l'étape `queue`.
- On ne signale ni ne compte comme vivant que ce qui correspond au triplet (`sid`, heure de démarrage, `boot_id`) : un pid ne prouve rien après un redémarrage.
- Le verrou global tenu par un processus mort est libéré par `flock`.
- `git worktree prune` nettoie les worktrees fantômes.

## 6. Côté laptop : le contrôleur Go

```
cmd/gatehaul/               main, routage des sous-commandes (stdlib flag)
internal/out/               sorties texte et JSON, codes stables, lignes Fix:
internal/clock/             horloge injectable : aucune attente réelle dans les tests
internal/ghx/               exécution de gh et git derrière une interface Runner (argv, stdin, timeout, jamais de shell), classification des erreurs de gh
internal/detect/            dépôt, remote, ref → SHA, état du working tree, détection de la gate
internal/doctor/            diagnostic cumulatif
internal/host/              Host ; interfaces Transport, Lifecycle et Backend
internal/host/codespaces/   Lifecycle (list/create/stop/delete) + Transport (gh codespace ssh -c N -- bash -s)
internal/host/fake/         Transport local bash -s dans un dossier temporaire (tests et CI)
internal/host/hosttest/     suite de conformité jouée contre chaque backend
internal/runner/            runner.sh embarqué, préambule, protocole, client
internal/bundle/            base incrémentale, git bundle create
internal/job/               ids, états, cache local des jobs
internal/lock/              verrou de fichier portable (gofrs/flock)
internal/patch/             assainissement (§7) et application des patchs
internal/ctl/               run, gen, apply, wait, log, up, down
```

**Dépendances externes :** `gofrs/flock` (verrou de fichier portable, Windows compris) et `golang.org/x/sys` (détection d'un vrai terminal). Ni SSH en Go, ni client API GitHub : `gh` fait tout, et c'est lui qui détient le token. `gatehaul` ne lit, ne stocke ni n'affiche jamais un token.

**Transport :** le client du runner sait si le script distant a démarré (ligne `HELLO`). Avant le démarrage (codespace qui se réveille, SSH refusé), il réessaie avec un délai croissant pendant au plus 5 minutes, sauf erreur permanente de `gh` (absent, non connecté, scope manquant), remontée tout de suite avec son `Fix:`. Après le démarrage, il ne réessaie jamais. Une erreur de `gh` ne prouve jamais l'absence d'un hôte. Les options SSH sont passées après `--` (`-T`, `BatchMode=yes`, `ForwardAgent=no`, `ClearAllForwardings=yes`, `ConnectTimeout=8`, `ServerAliveInterval=15`), les E/S sont bornées dans les deux sens, et l'arbre de processus de `gh` est tué à l'annulation (sous Windows, `gh` lance un `ssh` fils).

**Le backend *fake* est le deuxième backend :** il exécute le vrai runner dans un dossier local, ce qui permet de jouer toute la suite de conformité en CI sans Codespace.

## 7. Assainissement des patchs (`gen`, `apply`)

Le patch vient d'un hôte qui exécute du code écrit par un agent : c'est une **entrée hostile**. Il est assaini sur un **diff d'arbre**, jamais sur son texte.

1. Le sha256 des octets reçus doit être celui que le runner a annoncé.
2. Avant tout appel à git, les sections `GIT binary patch` sont lues : la taille déclarée d'un `literal` et la taille résultante d'un `delta` (lue dans l'en-tête zlib, sans tout décompresser) sont comparées aux plafonds, pour qu'un patch de quelques octets ne fasse pas gonfler git à plusieurs Gio.
3. Le patch est appliqué à un **index temporaire** (`GIT_INDEX_FILE`) et à un **répertoire d'objets temporaire** (`GIT_OBJECT_DIRECTORY`, avec les objets du dépôt en lecture seule par `GIT_ALTERNATE_OBJECT_DIRECTORIES`) : `read-tree <base>`, `apply --cached --binary`, `write-tree`. Rien n'est écrit dans le dépôt de l'utilisateur.
4. Le résultat est lu par `git diff-tree -r -z --raw --no-renames <base> <nouvel arbre>`, puis chaque entrée est validée :
   - modes limités à `100644` et `100755` : liens symboliques et sous-modules refusés ;
   - chemins relatifs, sans NUL, caractère de contrôle, antislash, `:`, composant `..`, préfixe de lecteur, ni point ou espace final ;
   - composant `.git` refusé sans tenir compte de la casse, ni des caractères ignorés par HFS+, ni des alias NTFS (`git~1`, flux `::$DATA`) ;
   - **alias 8.3 de Windows** refusés dans tout composant (`WORKFL~1`, `GATEHA~1.TOM`) ;
   - collisions de casse Unicode et conflits fichier/répertoire refusés, en interrogeant git répertoire par répertoire plutôt qu'en listant tout l'arbre ;
   - pointeurs LFS refusés ; plafonds par fichier (16 Mio), au total (64 Mio) et en nombre de fichiers (2000), appliqués aux nouveaux blobs seulement ;
   - fichier binaire détecté par un octet NUL dans les 8000 premiers octets (signalé, pas refusé).
5. **Chemins sensibles**, comparés sous forme normalisée (casse repliée, caractères ignorables retirés) : `gatehaul.toml`, `.claude/**`, `.codex/**`, `CLAUDE.md`, `AGENTS.md`, `.github/workflows/**`, `.githooks/**`, `.husky/**`, `Makefile`, `package.json`, `*.sh`, `.envrc`, `.gitattributes`, `.gitmodules`. Ils sont listés dans `sensitive_paths`. `gatehaul.toml`, les workflows et les hooks sont **refusés** sans `--allow-sensitive`.
6. Tous les problèmes sont listés, jamais seulement le premier. Un patch refusé sort en code 6 avec `patch_refused` et la liste ; rien n'est appliqué.

**Application :**

- Le patch est lié à son origine : le working tree doit être celui d'où le job est parti (`wrong_repo` sinon), et HEAD doit valoir le commit envoyé, ou en descendre sans avoir touché aux fichiers du patch (`stale_base`, code 6, sinon).
- Un verrou par job sérialise `gen`, `wait` et `apply` ; `applied.json` empêche toute double application, et un patch déjà présent dans l'arbre (vérifié par `git apply --reverse --check`) est reconnu comme appliqué.
- Avant d'écrire, les fichiers concernés sont copiés dans le cache du job, avec un marqueur `applying`. L'application elle-même ne peut pas être interrompue par Ctrl-C ; si elle échoue, ou si un marqueur reste après un plantage, les fichiers d'origine sont restaurés avant toute autre action.
- `git apply --check` puis `git apply --binary` écrivent dans le working tree, jamais dans l'index. Un conflit sort en code 6 avec `patch_conflict` et `Fix: … gatehaul apply <id>`.
