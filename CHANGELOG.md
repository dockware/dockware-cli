# Changes in PHPUnuhi

All notable changes of PHPUnuhi releases are documented in this file
using the [Keep a CHANGELOG](https://keepachangelog.com/) principles.

## [Unreleased]

### Added

- `clone`: copies a Shopware 6 shop over SSH into a local dockware environment with anonymised data; vendor/ is always installed with composer (PHP and node versions detected, composer credentials merged from every auth.json like composer does), every failure names its step and a step-specific hint, code excludes, size preflight, orders window and search-index skip; flags win over `.dockware-clone.yml` also when they switch a setting off (`--orders-since-days 0`, `--skip-search-index=false`, `--max-code-size -1`). See docs/clone.md.
- `clone`: PHP extensions the server has and the container lacks (`php -m` from the probe, e.g. zstd, imagick, memcached) are installed via apt (`php<ver>-<ext>`) and php-fpm restarted before composer and the import; profilers/debuggers are skipped, extensions without a package stay missing with a warning.
- `clone`: the local MySQL is tuned for the import and the indexers and the values are persisted in the db volume (`SET PERSIST`: buffer pool a quarter of the container's RAM up to 4 GiB, 2 GiB redo log, `innodb_flush_log_at_trx_commit=2`); the import session writes no binlog. `SHOPWARE_ADMIN_ES_ENABLED` and `SHOPWARE_ADMIN_ES_REFRESH_INDICES` are set to `0` locally instead of being copied from the server.
- `clone`: the dump runs in the background while code, container, PHP extensions and composer are prepared (a dump failure cancels them and is reported as the dump error); the import splits the dump in the container (all DDL first, then 128 MiB row parts through up to 8 mysql sessions, largest first, views and triggers last) with the InnoDB redo log disabled for its duration; loaders of a cancelled or failed import are stopped before the next one; verify runs its pattern counts in 4 parallel batches.
- `clone`: the local shop runs with `APP_DEBUG=0` (`1` with `--app-env dev`) instead of the server's value, and a `php-clone.ini` (opcache 512 MB / 100000 files, `revalidate_freq=2`, larger realpath cache) is mounted for php-fpm and cli.
- `clone`: all ssh and rsync calls of a run share one connection (`ControlMaster`, not on Windows; `--ssh-option ControlMaster=no` turns it off), and the ssh connect timeout is 60 s, so servers behind slow tunnels no longer fail in the banner exchange.

## [0.7]

### Fixed

- Fixed building problem on Windows. Removed the interactive-terminal check.

## [0.6]

### Added

- Initial version

