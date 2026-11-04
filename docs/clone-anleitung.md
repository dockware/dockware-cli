# dockware clone – Anleitung

Schritt für Schritt: einen Shopware-Shop vom Server als lokale
dockware-Umgebung mit anonymisierten Daten aufsetzen. Alle Optionen und
Details stehen in der Referenz [`clone.md`](clone.md).

## Was passiert

1. Der Server wird per ssh gelesen: Shopware-, PHP-, MySQL- und Node-Version,
   PHP-Extensions, Schema, Domains, Namen der `.env`-Keys.
2. Die Datenbank wird **auf dem Server** anonymisiert gedumpt (Standard
   `stream`: keine Schreibzugriffe außer einem temporären shopware-cli in
   `/tmp`). Personenbezogene Daten verlassen den Server nie im Klartext.
3. Der Code kommt aus einem lokalen Checkout (`--code`) oder per rsync vom
   Server, ohne `vendor/`, Uploads, Backups und Secrets.
4. Lokal startet `dockware/shopware-essentials` mit der PHP-Version des
   Servers; `composer install` läuft im Container mit der ermittelten PHP- und
   Node-Version. `vendor/` wird nie heruntergeladen.
5. Der Dump läuft im Hintergrund, während Code, Container, PHP-Extensions und
   `composer install` vorbereitet werden. Der Import teilt den Dump im
   Container: erst alle Tabellen anlegen, dann die Zeilen in Teilen über bis
   zu 8 MySQL-Verbindungen parallel.
6. Prüfung der Anonymisierung, Domains auf `localhost`, Admin-User
   `admin` / `shopware`, Theme-Compile, Smoke-Tests.
7. Medien werden nicht kopiert, sondern per Redirect von der Quelle geladen.
8. Lokal gilt `APP_DEBUG=0` (mit `--app-env dev`: `1`) und ein größerer
   Opcache (`<name>/php-clone.ini`); in einem großen Shop unter macOS
   30–55 % kürzere Antwortzeiten der Seiten.

## Voraussetzungen

- Docker läuft (`docker info`).
- ssh ohne Rückfrage: `ssh <alias> true` muss durchlaufen. Ein gesperrter
  1Password-Agent zuerst entsperren.
- Auf dem Server: `rsync`, `php`, `mysql`, `gzip`.
- Composer-Zugangsdaten für private Pakete / Shopware-Store: ein `auth.json`
  im Checkout, in `~/.composer/` oder `~/.config/composer/`, oder
  `--composer-auth <datei>`. Alle gefundenen Quellen werden wie bei composer
  zusammengeführt.

## Binary bauen

Go muss nicht installiert sein:

```bash
cd ~/dockware-cli   # Checkout von github.com/dockware/dockware-cli
docker run --rm -v "$PWD":/src -v dwc-gocache:/root/.cache -v dwc-gomod:/go/pkg/mod \
  -w /src golang:1.22 sh -c 'GOOS=darwin GOARCH=arm64 go build -o build/dockware-cli .'
alias dockware-cli=~/dockware-cli/build/dockware-cli
```

## Neues Projekt aufsetzen

1. **ssh-Ziel und Pfad klären.** Ziel ist ein Alias aus `~/.ssh/config`,
   `user@host` oder `ssh://user@host:port`. `--path` ist das Projekt-Root auf
   dem Server (dort liegen `bin/console` und `composer.json`).

   ```bash
   ssh shop-stage 'ls /var/www/shop/bin/console /var/www/shop/composer.json'
   ```

2. **Stage statt Live nutzen.** Der Klon liest nur, lädt aber shopware-cli
   nach `/tmp` und erzeugt Last durch den Dump.

3. **Config anlegen** (optional), am besten **außerhalb** des Checkouts, damit
   er git-clean bleibt, z. B. `~/clones/<projekt>-clone.yml`. Vorlage:
   [`dockware-clone.example.yml`](dockware-clone.example.yml). Liegt die Datei
   als `.dockware-clone.yml` im Checkout, wird sie mit `--code` automatisch
   gefunden.

4. **Starten**, in einem Arbeitsordner außerhalb des Checkouts (dort entsteht
   `./<name>/` mit Compose-Datei, Dump und `clone.log.json`):

   ```bash
   mkdir -p ~/clones && cd ~/clones
   dockware-cli clone shop-stage \
     --path /var/www/shop \
     --code ~/projects/shop \
     --config ~/clones/shop-clone.yml \
     --name dwc-shop --port 8300
   ```

   Ohne `--code` wird der Code vom Server geholt (Abbruch über 300 MB, siehe
   „Ausschließen“).

5. **Ergebnis.** Am Ende stehen Shop-URL, Admin-URL, Domain-Mapping,
   Store-API-Access-Keys und der Compose-Befehl. Exit-Code 0 heißt: Import,
   Anonymisierungs-Prüfung und Smoke-Tests sind grün.

## Ausschließen

| Was | Config | Flag |
|---|---|---|
| Ordner/Dateien beim Code-Sync (nur ohne `--code`) | `exclude:` (rsync-Muster) | `--exclude '/custom/static-plugins/Alt/'` (mehrfach) |
| Größenlimit Code-Sync | `max_code_size_mb: 500` | `--max-code-size 500`, `-1` = kein Limit |
| Tabellen ohne Zeilen (auch Core) | `no_data: [tabelle, prefix_*]` | – |
| Extension-Tabellen **mit** Zeilen | `allow_tables: [tabelle]` | – |
| Nur Bestellungen der letzten N Tage | `orders_since_days: 90` | `--orders-since-days 90`, `0` = alle |
| Suchindex lokal neu aufbauen statt übertragen | `skip_search_index: true` | `--skip-search-index` |
| Spalten zusätzlich anonymisieren | `rules:` | – |

- rsync-Muster: `/` am Anfang = ab Projekt-Root, `/` am Ende = nur Ordner,
  `**` geht über Unterordner.
- Eingebaut ausgeschlossen: `.env*`, `auth.json`, `config/jwt/`, `vendor/`,
  `var/`, `public/media|thumbnail|theme|bundles|build/`, `files/`, `uploads/`,
  `import/`, `export/`, `backup*/`, `dump*/`, Archive, `*.sql`, `*.log`,
  `*.csv`/`*.xlsx` (außer Fixtures unter `Resources/` und `tests/`), Dateien
  über 50 MiB.
- Extension-Tabellen sind ohne Zeilen, solange sie nicht in `allow_tables`
  stehen. Der Lauf listet sie mit ungefährer Zeilenzahl. Nur Tabellen ohne
  personenbezogene Daten freigeben.
- Flags schlagen die Config, auch beim Abschalten (`--orders-since-days 0`,
  `--skip-search-index=false`).

## Versionen

- **PHP**: `--php` → `php:` in der Config → Server.
- **PHP-Extensions**: Was `php -m` auf dem Server listet und im Container
  fehlt, wird per apt nachinstalliert (`php8.3-zstd`, `php8.3-imagick`, …),
  danach php-fpm-Neustart. Profiler/Debugger (tideways, xdebug, pcov, …)
  werden übersprungen. Ohne Paket oder ohne Netz bleibt die Extension mit
  Warnung fehlend.
- **Node im Shop**: `--node` → `node:` → `.nvmrc` / `.node-version` /
  `package.json` im Checkout → `node -v` auf dem Server. Fehlt die Version im
  Image, wird sie per nvm installiert.
- **Frontend-Image** (headless): `frontend.image` → `--node` / `node:` →
  Erkennung im Frontend-Ordner bzw. Checkout → Server → `node:lts`.
- Nach `docker compose down` + `up` ist wieder die Standard-Node des Images
  aktiv und die nachinstallierten Extensions sind weg; Klon erneut ausführen
  oder von Hand: `nvm` bzw. als root `apt-get install php8.3-<ext>`.

## Fehler

Jeder Fehler nennt den Schritt, die Ausgabe des Tools und einen Hinweis. Der
Exit-Code ist dann ungleich 0; dasselbe steht in `<name>/clone.log.json`
(`error_step`, `error_hint`).

```
Error: [ssh] ssh to dockware@127.0.0.1 failed: exit status 255
  output:
    dockware@127.0.0.1: Permission denied (publickey,password).
  hint: the server rejected the ssh login: check user and host (ssh config alias), pass --identity <key>, or authorise your public key on the server
```

```
Error: [config] --orders-since-days must be 0 (all orders) or more, got -1
  hint: pass the number of days whose orders are kept, or 0 for all orders
```

| Schritt | Häufige Ursache | Lösung |
|---|---|---|
| `config` | unbekannter Key, ungültiger Flag-Wert | Meldung nennt Key/Flag |
| `ssh` | Key abgelehnt, Agent gesperrt, Host unbekannt, Timeout | `ssh <alias> true` testen, `--identity`, VPN |
| `probe` | falscher `--path`, kein Shopware-Projekt, keine Leserechte | Pfad prüfen |
| `database` | DB-Zugang aus Server-`.env` abgelehnt / nicht erreichbar | `DATABASE_URL` auf dem Server |
| `shadow` | DB-User darf kein Schema anlegen | Standard `--strategy stream` nutzen |
| `upload` / `dump` | `/tmp` voll oder `noexec` | `/tmp` freigeben, `--strategy shadow` |
| `rsync` | Leserechte (Exit 23), Projekt zu groß | `exclude:`, `--max-code-size`, `--code` |
| `docker` | Docker aus, Port belegt, Image-Pull, Node-Download | Docker starten, `--port`, Netzwerk/Proxy |
| `composer` | fehlende Zugangsdaten, PHP-Version, kein Netz | `auth.json` / `--composer-auth`, `--php` |
| `import` / `verify` / `post-import` / `smoke` | SQL-Fehler, ungeprüfte Spalte, Console-Befehl, Seite nicht 200 | Ausgabe lesen, `docker compose -f <name>/docker-compose.yml logs shop` |

Vollständige Tabelle: [`clone.md` → Errors](clone.md#errors).

## Aufräumen

```bash
docker compose -f ~/clones/<name>/docker-compose.yml --profile frontend down -v
rm -rf ~/clones/<name>
```

Ein Checkout aus `--code` wird nicht verändert.

## Beispiel: großer Shop (Stage)

Stand der Prüfung am Server: Shopware 6.6.10, PHP 8.3, Node 18, MySQL 8.4,
Datenbank ~36 GB (davon ~12 GB Analytics-/Extension-Tabellen, die ohne
Zeilen kommen), ~166.000 Produkte, ~860 Bestellungen.

```bash
mkdir -p ~/clones && cd ~/clones
dockware-cli clone shop-stage \
  --path /var/www/shop \
  --code ~/projects/shop \
  --config ~/clones/shop-clone.yml \
  --name dwc-shop --port 8300
```

`~/clones/shop-clone.yml` schließt Telemetrie und Thumbnails aus und überträgt
die abgeleiteten Daten (Preise, Kategoriebaum, Suchindex) mit, denn ihr
lokaler Neuaufbau dauerte über 2 Stunden. Ohne die Produkt-Zusatztabellen
eines Projekt-Plugins antwortete jede Produktseite mit 400 (`CartException:
Payload key ... not found`) — solche Tabellen in `allow_tables` aufnehmen.
`product_warehouse*` (Shopware Commercial, ~23 Mio. Zeilen) ist dort
kommentiert. Lauf: ~8 min inkl. Dump.
