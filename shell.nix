{ pkgs ? import <nixpkgs> {
    # libolm is only needed by one client API test (mautrix crypto). nixpkgs marks it insecure,
    # so it is allowed here by name and used for tests only, never in a release build.
    config.permittedInsecurePackages = [ "olm-3.2.16" ];
  }
}:

# Development shell for Dendrite. Usage: run `nix-shell` in the repository root.
#
# Postgres-backed tests: run `dev-postgres start` once inside the shell. It starts a
# throwaway server on a unix socket in ./.pgsock (data in ./.pgdata, both git-ignored).
# `dev-postgres stop` shuts it down again.
let
  devPostgres = pkgs.writeShellScriptBin "dev-postgres" ''
    set -eu
    root="''${DENDRITE_ROOT:-$PWD}"
    data="$root/.pgdata"
    sock="$root/.pgsock"
    case "''${1:-}" in
      start)
        mkdir -p "$sock"
        if [ ! -d "$data" ]; then
          initdb -D "$data" --auth=trust >/dev/null
        fi
        if pg_ctl -D "$data" status >/dev/null 2>&1; then
          echo "postgres already running"
        else
          pg_ctl -D "$data" -l "$root/.pgdata.log" -o "-k $sock -c listen_addresses=" -w start
        fi
        ;;
      stop)
        pg_ctl -D "$data" -m fast stop
        ;;
      *)
        echo "usage: dev-postgres start|stop" >&2
        exit 1
        ;;
    esac
  '';
in
pkgs.mkShell {
  packages = [
    (pkgs.go_1_25 or pkgs.go)
    pkgs.gopls
    pkgs.gotools
    pkgs.golangci-lint
    pkgs.gcc
    pkgs.pkg-config
    pkgs.sqlite
    pkgs.olm
    pkgs.postgresql
    pkgs.git
    pkgs.gnumake
    pkgs.curl
    pkgs.jq
    devPostgres
  ];

  # The sqlite3 driver (mattn/go-sqlite3) needs cgo.
  CGO_ENABLED = "1";

  shellHook = ''
    export DENDRITE_ROOT="$PWD"
    # nix-shell sets a TMPDIR long enough to break unix socket paths (108 byte limit) in tests.
    export TMPDIR=/tmp
    export GOPATH="''${GOPATH:-$HOME/go}"
    export PATH="$GOPATH/bin:$PATH"
    # test/db.go reads POSTGRES_HOST for its connection string; createdb reads PGHOST.
    export POSTGRES_HOST="$PWD/.pgsock"
    export PGHOST="$PWD/.pgsock"
    echo "dendrite dev shell: $(go version)"
    echo "  unit tests:  go test ./syncapi/...        (sqlite only: -run '/sqlite')"
    echo "  postgres:    dev-postgres start|stop      (needed for the /postgres subtests)"
    echo "  lint:        golangci-lint run ./syncapi/..."
  '';
}
