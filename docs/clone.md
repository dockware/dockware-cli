# dockware clone

`dockware clone` mirrors a running Shopware 6 shop into a local dockware
docker-compose environment with anonymised data.

```
dockware clone <ssh-target> --path <project root on the server> [flags]
```

## What it does

1. **Probe** the server over ssh: Shopware, PHP, MySQL and node versions, PHP
   extensions, the schema, core tables, sales channel domains, `.env` keys. No
   row of a personal-data table is read.
2. **Dump** the database anonymised *on the server*. With the default
   `stream` strategy a static shopware-cli is uploaded to a private temp dir and
   rewrites personal columns inside the `SELECT`; the compressed stream goes
   straight into `<out>/dump.sql.zst`. With `--strategy shadow` the anonymised
   rows are copied into a temporary schema first, then dumped with `mysqldump`.
   The dump runs in the background while steps 3–5 prepare the local side; a
   failure on either side cancels the other.
3. **Code**: a local checkout (`--code`) or an rsync of the project without
   runtime data, uploads, backups and secrets.
4. **docker compose up** of `dockware/shopware-essentials` with the server's
   PHP version, plus Redis when the project uses it.
5. **composer install** in the container (never a vendor copy) and, once the
   dump is there, the database import run in parallel. The import splits the
   dump inside the container: all DDL first in one session (concurrent
   `CREATE TABLE` with foreign keys deadlocks), then the rows in 128 MiB parts,
   largest first, through up to 8 mysql sessions, views and triggers last so
   no trigger fires on the loaded rows. Loaders a cancelled run left in the
   container are stopped on failure and before the next import. The InnoDB
   redo log is disabled during the import (MySQL 8.0.21+; re-enabled
   afterwards, also on failure); a crash in that window loses the local
   database, rerun the clone.
   The local MySQL is tuned for the import and the indexers and the settings
   are persisted in the db volume (`SET PERSIST`): buffer pool a quarter of the
   container's RAM up to 4 GiB, 2 GiB redo log,
   `innodb_flush_log_at_trx_commit=2`, the import sessions write no binlog and
   skip unique checks.
6. **Verify** that the imported data is anonymised: local values must match the
   generated patterns, and sampled values must not exist on the server. The
   pattern counts run in up to 4 parallel batches.
7. Make it run locally: domains rewritten to `localhost` / `<slug>.localhost`,
   admin user `admin` / `shopware`, staging mode, `plugin:refresh`,
   `assets:install`, `theme:compile`, optional `post_sql`.
8. Optional headless frontend from the config, then **smoke tests**:
   storefront, Store API, Admin API login, frontend SSR.

Local runtime settings: `APP_DEBUG=0` (`1` with `--app-env dev`) instead of
the server's value, since the debug container re-checks every file on the
bind mount (about half of the storefront response time on macOS); a
`php-clone.ini` (fpm and cli) raises opcache to 512 MB / 100000 files with
`revalidate_freq=2`, so code edits show up within 2 s. Both can be overridden
via `env:` or by editing `<out>/php-clone.ini`.

What never leaves the server: personal data (anonymised before it is
serialised), `.env` secrets (only the key names are read; secret values are set
empty locally), `vendor/` (installed locally with composer), media (served from
the source via an Apache redirect).

Connection keys are rewritten for the local stack: `DATABASE_URL`,
`MAILER_DSN`, `OPENSEARCH_URL` and the messenger DSNs get fixed local values,
`REDIS*` keys ending in `HOST` / `PORT` become `redis` / `6379`, `REDIS*` keys
containing `URL` or `DSN` (`REDIS_URL`, `REDIS_DSN_CACHE`,
`REDIS_DSN_CACHE_HTTP`, …) become `redis://redis:6379`, and `REDIS*` keys
containing `PASS` are set empty. For other secret-looking keys whose value is
a URL (`CART_DSN`, `SESSION_DSN`, `SMOXY_URL`, …) only the scheme leaves the
server; keys with a `redis`, `rediss`, `valkey` or `valkeys` scheme are pointed
at the local redis, every other scheme stays empty.

## Quick start for a new project

Prerequisites:

- Docker running locally (`docker info` works). The clone checks this before it
  touches the server, also for `--dump-only` with the `stream` strategy, which
  takes shopware-cli out of its image.
- ssh access that works without a prompt: `ssh <alias> true` must succeed
  (key in the agent or `--identity`). A 1Password/ssh agent that asks for
  approval has to be unlocked first.
- On the server: `rsync`, `php`, `mysql`/`mysqldump`, `gzip`. `zstd` is not
  needed; shopware-cli brings its own compression.
- `rsync` locally.
- Composer credentials if the project uses private packages or store plugins.
  Like composer, the clone merges every `auth.json` it finds, per section
  (`http-basic`, `bearer`, `github-oauth`, …) and host; on a conflict the
  earlier source wins: `--composer-auth <file>`, the `COMPOSER_AUTH`
  variable, `auth.json` in the checkout, `$COMPOSER_HOME/auth.json`,
  `~/.composer/auth.json`, `~/.config/composer/auth.json`. The clone prints
  which sources it used. Private git packages use your ssh agent, which is
  forwarded into the container.

Commands:

```bash
# with a local checkout (fastest; the checkout is bind-mounted, nothing is written into it)
dockware clone shop-stage --path /var/www/shop --code ~/projects/shop

# without a checkout: the code is synced from the server into <out>/code
dockware clone shop-stage --path /var/www/shop

# recent orders only, search index rebuilt locally instead of transferred
dockware clone shop-stage --path /var/www/shop --orders-since-days 90 --skip-search-index

# headless frontend: configure frontend: in .dockware-clone.yml (see below), then
dockware clone shop-stage --path /var/www/shop --code ~/projects/shop

# only the anonymised dump, no local environment
dockware clone shop-stage --path /var/www/shop --dump-only

# shadow strategy (needs CREATE DATABASE or an existing empty <db>_dwc_shadow schema)
dockware clone ssh://deploy@shop.example.com:2222 --path /var/www/html --strategy shadow
```

`<ssh-target>` is an ssh config alias, `user@host` or `ssh://user@host:port`.
The output directory defaults to `./dwc-<target>` and holds the compose file,
the dump, env files and `clone.log.json`. It must not lie inside the checkout.

All ssh and rsync calls of a run share one connection (OpenSSH
`ControlMaster`, socket in a private `/tmp/dwc-ssh-*` directory, closed at the
end), so only the first call pays the handshake; the connect timeout is 60 s
for slow tunnels and jump hosts. `--ssh-option` values win over these
defaults: `--ssh-option ControlMaster=no` turns sharing off, and a
`ControlMaster` or `ControlPath` of your own replaces it. On Windows, whose
OpenSSH has no `ControlMaster`, every call connects anew.

At the end the command prints the shop URL, the admin URL, the domain mapping,
the Store API access keys and the compose command.

## Configuration: `.dockware-clone.yml`

Found at `<code>/.dockware-clone.yml` when `--code` is given, or passed with
`--config`. Unknown keys are rejected. See
[`dockware-clone.example.yml`](dockware-clone.example.yml) for a commented
example.

| Key | Type | Meaning |
|---|---|---|
| `allow_tables` | list of globs | Extension tables whose rows are exported. Everything outside the Shopware core is exported schema-only unless listed here (`swag_cms_extensions_*`, `swag_language_pack_*`, `swag_customized_products_template*`, `swag_social_shopping_sales_channel` are built in). |
| `no_data` | list of globs | Additional schema-only tables, also for core tables. Built in: cart, queues, logs, sessions, tokens, documents, import/export files, `*_log`, `*_logs`, `*_log_entry`. |
| `rules` | list of `{table, column, kind}` | Column rewrites on top of the defaults. A project rule for a column that already has a default replaces it. Rules for columns missing in the server schema are skipped. |
| `env` | map | Overrides for the generated local environment (`shop.env`). Use it for keys the clone set empty because they looked like credentials. Keys the dockware image controls (`PHP_VERSION`, `NODE_VERSION`, `NVM_VERSION`, `TZ`, `XDEBUG_*`, …) are never written, neither from the server `.env` nor from here (a warning names them); use `php:` / `node:` instead. |
| `post_sql` | path | SQL file run after the import, relative to the config file; e.g. sandbox payment keys. |
| `exclude` | list of rsync patterns | Additional paths the code sync skips (see below). Ignored with `--code`. |
| `max_code_size_mb` | int | Abort the code sync when the project on the server is larger (decimal MB). Default 300, `-1` = no limit. |
| `orders_since_days` | int | Keep only orders of the last n days and their child rows. `0` = all orders. |
| `skip_search_index` | bool | Export `product_search_keyword` and `product_keyword_dictionary` schema-only and rebuild them locally with `dal:refresh:index --only=product.indexer`. |
| `php` | `major.minor` | PHP version of the local shop; default: the server's version. |
| `node` | major | Node major version used in the shop container and for the frontend image; default: detected (see Versions). |
| `frontend` | map | Headless frontend started next to the shop, see below. |

Command line flags win over the config, also when they switch a setting off:
`--orders-since-days 0` exports all orders despite `orders_since_days: 90`,
`--skip-search-index=false` transfers the index despite `skip_search_index:
true`, `--max-code-size -1` lifts a configured limit. `--orders-since-days`
rejects negative values, `--max-code-size` accepts `-1` or a size above 0.
`exclude` and `--exclude` are combined.

### Rules

```yaml
rules:
  - { table: acme_newsletter_lead, column: email, kind: email }
  - { table: acme_newsletter_lead, column: phone, kind: phone }
  - { table: order, column: customer_comment, kind: null }
```

Kinds: `email`, `first_name`, `last_name`, `company`, `street`, `zip`, `city`,
`phone`, `ip`, `username`, `token`, `password`, `empty_json_array`, `null`.
Values are derived from the row id, so the same row always gets the same
replacement and foreign relations stay plausible. `NULL` and empty values are
left as they are. `password` sets the bcrypt hash of `shopware`, so every
account (admin users, customers, integrations) logs in with that password.
`null` clears the value (`NULL`, `''` or `{}` depending on the column).

The default rules cover customers, addresses, order customers/addresses,
newsletter recipients, product reviews, promotion codes, admin users, access
keys, integrations and app secrets. `system_config` rows whose key looks like a
credential (`password`, `secret`, `token`, `apikey`, …) are dropped; use
`post_sql` to add sandbox keys.

### Frontend

```yaml
frontend:
  path: frontend            # directory in the code tree, mounted to /app
  image: node:22            # optional; default node:<major> from --node/node:, else the frontend dir, the code root or the server
  install: npm ci           # optional, runs before command
  command: npm run dev      # required
  port: 3000                # port inside the container, published on --frontend-port
  env:
    NUXT_PUBLIC_SHOPWARE_ENDPOINT: ${SHOP_URL_INTERNAL}/store-api
    NUXT_PUBLIC_SHOPWARE_ACCESS_TOKEN: ${ACCESS_KEY:Headless}
    NUXT_PUBLIC_SITE_URL: ${FRONTEND_URL}
```

Placeholders in `env`: `${SHOP_URL}` (browser URL of the shop),
`${SHOP_URL_INTERNAL}` (`http://shop`, for server-side requests inside the
compose network), `${FRONTEND_URL}` (`http://localhost:<frontend-port>`),
`${ACCESS_KEY}` (first storefront/headless sales channel) and
`${ACCESS_KEY:<sales channel name>}`. `node_modules` and `.nuxt` live in
volumes, so the checkout stays clean. `--no-frontend` skips it.

## Excluding

### Code

Built-in rsync excludes (anchored paths refer to the project root):

- secrets and state: `/.env`, `/.env.*`, `/install.lock`, `/auth.json`,
  `/config/jwt/`
- tooling: `.git/`, `.idea/`, `.DS_Store`, `node_modules/`, `.nuxt/`, `.output/`
- installed or generated: `/vendor/`, `/var/`, `/public/media/`,
  `/public/thumbnail/`, `/public/sitemap/`, `/public/theme/`,
  `/public/bundles/`, `/public/build/`
- data and backups: `/files/`, `/uploads/`, `/import/`, `/export/`,
  `/backup*/`, `/dump*/`, `*.sql`, `*.sql.zst`, `*.gz`, `*.zip`, `*.tar`,
  `*.tar.*`, `*.tgz`, `*.bak`, `*.log`, `*.pem`, `*.key`
- `*.csv`, `*.xlsx`, except below `/custom/` and `/src/` (plugin fixtures)

Single files larger than 50 MiB are skipped. They are listed only when the
size preflight runs: before the transfer a dry run measures the tree; above
`max_code_size_mb` (default 300) the clone stops and names the largest
directories, so you can exclude them. With `-1` (no limit) there is no
preflight, so oversized files are skipped without being listed.

`exclude:` and `--exclude` take rsync patterns, one per entry:

```yaml
exclude:
  - /public/catalogs/          # a directory below the project root
  - /custom/static-plugins/Legacy*/
  - "*.psd"                    # a file pattern anywhere
  - "**/fixtures/large/"       # a directory at any depth
```

A leading `/` anchors the pattern at the project root, a trailing `/` matches
only directories, `*` does not cross `/`, `**` does. Excludes only apply to the
sync from the server; with `--code` the checkout is used as it is.

### Tables

- `no_data:` adds schema-only tables (globs, core tables allowed).
- Extension tables are schema-only unless they match `allow_tables:`. The
  denied tables and their row counts are printed and written to the audit.
- `orders_since_days` / `--orders-since-days N` keeps only orders whose
  `order_date_time` is within the last N days, together with their child rows
  (line items, deliveries, transactions, addresses, order customers, …). The
  cutoff is computed once (UTC, at day granularity) and used for every table,
  so a kept order keeps all its rows; it is part of the rules hash in the
  audit. A source without an `order.order_date_time` column gets a warning
  and all orders.
- `skip_search_index` / `--skip-search-index` leaves the product search index
  out of the dump; it is rebuilt locally after the import (`search-index`
  phase). Only worth it for small catalogs: the import of the exported rows
  takes seconds, `product.indexer` on ~166,000 products over two hours. The
  same holds for nulling derived columns (`cheapest_price*`) or exporting
  `product_category_tree` schema-only: the storefront needs them, and only
  the indexer rebuilds them.

`--limit table:rows` (stream strategy only) caps rows per table via
shopware-cli; it creates staging tables on the server.

## Versions

- **PHP**: the server's `major.minor`, override with `--php` or `php:`. The
  dockware image switches PHP at start (`PHP_VERSION`).
- **PHP extensions**: every extension the server's `php -m` lists and the
  container lacks is installed from the image's apt sources
  (`php<ver>-<ext>`, e.g. `php8.3-zstd`, `php8.3-imagick`) and php-fpm is
  restarted, before composer checks the platform. Profilers and debuggers
  (tideways, xdebug, pcov, blackfire, newrelic, opcache, ddtrace, excimer)
  are skipped. An extension without a package, or all of them when apt has
  no network, stay missing with a warning per extension; code paths using
  them fail until the extension is in the image.
- **Node** for the shop container (theme and admin builds): `--node`, else
  `node:` in the config, else the first of `.nvmrc`, `.node-version`,
  `package.json` (`volta.node`, `engines.node`) in the code root, else
  `node -v` on the server. The version is installed via nvm if missing and
  made the default; the effective `node -v` is printed.
- **Frontend image**: `frontend.image`, else `node:<major>` from `--node` /
  `node:`, else detected from the frontend directory, then the code root, then
  the server's node, else `node:lts`.

The node version and the installed PHP extensions live inside the shop
container (nvm under `/var/www/.nvm`, apt packages; no volume). They survive
`docker compose stop` / `start`, but after `docker compose down` and `up` the
image's default node is active again and the extensions are gone. Re-run the
clone to set them up again, or by hand:
`docker compose -f <out>/docker-compose.yml exec shop bash -lc 'nvm install <major> && nvm alias default <major>'`
and `docker compose -f <out>/docker-compose.yml exec -u root shop bash -c 'apt-get update && apt-get install -y php8.3-<ext> && service php8.3-fpm restart'`.

The clone prints the result and where each value came from, e.g.
`PHP 8.3 (server), Node 22 (checkout)`.

## Media

Media and thumbnails are not copied. Apache redirects requests for files that
do not exist locally to the public media URL of the source (from
`config/packages/*.yaml` `filesystem.public.url`, else `APP_URL`). Local
uploads and an existing `public/media` in a checkout take precedence. Without a
usable URL a warning is printed and missing media 404s.

## Errors

Every failure is reported as `Error: [<step>] <message>`, followed by the tail
of the failing tool's output and a `hint:` line. Hints are chosen per step, so
a `Connection refused` from composer is not explained as an ssh problem. The
exit code is non-zero, also when the shop came up but verification or smoke
checks failed. The audit file `<out>/clone.log.json` holds the same
information in `error`, `error_step` and `error_hint`, together with phases,
warnings and all checks.

```
Error: [ssh] ssh to deploy@shop.example.com failed: exit status 255
  output:
    deploy@shop.example.com: Permission denied (publickey,password).
  hint: the server rejected the ssh login: check user and host (ssh config alias), pass --identity <key>, or authorise your public key on the server
```

```
Error: [shadow] the database user shop@% may not create the shadow schema shop_dwc_shadow
  hint: use --strategy stream (default, needs no server writes) or ask the hoster for CREATE on that schema
```

Warnings (`WARN …`) do not stop the clone; they are also stored in the audit.
One example: `orders_since_days` is set but the source has no
`order.order_date_time` column, so all orders are exported and the audit
records `orders_since_days = 0`.

| Step | Typical cause | What to do |
|---|---|---|
| `config` | unknown config key, bad rule kind, output dir inside the checkout, invalid `--php`/`--node`/`--exclude`, negative `--orders-since-days`, `--max-code-size` below `-1` or `0`, unreadable `--composer-auth` | fix the config or flag; the message names the key |
| `ssh` | agent refused / `signing failed` (1Password locked), `Permission denied (publickey)`, host key unknown or changed, timeout, unresolved host, connection lost while a command ran | unlock the agent (`ssh-add -l`), `--identity <key>`, connect once with `ssh <target>`, check VPN/firewall/`--ssh-port` |
| `probe` | `--path` is wrong, no Shopware project there (`vendor/shopware/core` missing), files not readable, `php`/`mysql` missing on the server | check `--path`, permissions and the tools on the server |
| `database` | `Access denied` for the credentials in the server `.env`, MySQL not reachable from the ssh host | fix `DATABASE_URL` on the server |
| `shadow` | the DB user may not `CREATE DATABASE`, `binlog_format=STATEMENT`, shadow schema exists and is not empty | use `--strategy stream`, or let the hoster create an empty `<db>_dwc_shadow` |
| `upload` | `/tmp` not writable or mounted `noexec`, binary does not run on the server | `--strategy shadow` |
| `dump` | shopware-cli or mysqldump failed, disk full | see the output; free `/tmp` on the server |
| `rsync` | files not readable (`Permission denied`, exit 23), project larger than `max_code_size_mb`, rsync missing on the server | fix permissions or add `exclude:`; raise `--max-code-size`; `--code <checkout>` |
| `docker` | daemon not running (also checked for `--dump-only` with `stream`), port in use, image pull failed, container did not become healthy, nvm cannot download the `--node` version | start Docker Desktop; `--port` / `--frontend-port`; check `--image`; check Docker's network/proxy or pick a `--node` the image ships |
| `composer` | `Authentication required` (packages.shopware.com, private repos), private git package without ssh key, lock file needs another PHP version or extension, packagist not reachable from the container | `--composer-auth` / an `auth.json` in one of the merged locations / `COMPOSER_AUTH`; load the key into the agent; `--php`; check Docker's network/proxy |
| `import` | SQL error while loading the dump (MySQL version mismatch, disk full), local MySQL not up | see the SQL error in the output; `docker compose -f <out>/docker-compose.yml logs shop` |
| `verify` | a column still carries server values (missing rule); local MySQL not reachable | add a `rules:` entry; do not share the clone |
| `post-import` | `bin/console` step failed (plugin refresh, theme compile, search index) | run the printed command in the container |
| `frontend` | frontend container failed, placeholder refers to a missing sales channel | check `frontend:` config and `docker compose logs frontend` |
| `smoke` | storefront, Store API, Admin API or frontend SSR not answering 200 | `docker compose -f <out>/docker-compose.yml logs shop` |

## Clean up

```bash
docker compose -f <out>/docker-compose.yml down -v
rm -rf <out>
```

`down -v` removes the database, vendor and cache volumes; the checkout given
with `--code` is untouched.
