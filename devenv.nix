{ pkgs, ... }:
{
  languages.go.enable = true;

  packages = [
    pkgs.gopls
    pkgs.gotools
    pkgs.golangci-lint
    pkgs.sqlite
    pkgs.sops
    pkgs.age
    pkgs.secretspec
    pkgs.tailscale
    pkgs.docker-client
    pkgs.docker-compose
    pkgs.jq
    pkgs.yq-go
    pkgs.nixfmt-rfc-style
    pkgs.shellcheck
  ];

  scripts = {
    celine.exec = "go run ./cmd/celine \"$@\"";
    test.exec = "go test ./...";
    lint.exec = "go vet ./...";
    render.exec = "go run ./cmd/celine render --check";
  };

  enterShell = ''
    echo "celine dev shell: celine validate, celine doctor, celine render --check"
  '';
}
