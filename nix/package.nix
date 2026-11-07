{
  lib,
  buildGoModule,
  makeWrapper,
  sqlite,
  sops,
  secretspec,
}:

buildGoModule rec {
  pname = "celine";
  version = "0.1.0";

  src = ../.;

  # Dependencies are vendored in the repository (see go.mod).
  vendorHash = null;

  subPackages = [ "cmd/celine" ];

  ldflags = [
    "-s"
    "-w"
    "-X"
    "main.version=${version}"
  ];

  nativeBuildInputs = [ makeWrapper ];

  # The CLI shells out to sqlite3, sops, and secretspec at runtime.
  # docker and tailscale are intentionally host-provided.
  postInstall = ''
    wrapProgram $out/bin/celine \
      --prefix PATH : ${
        lib.makeBinPath [
          sqlite
          sops
          secretspec
        ]
      }
  '';

  meta = {
    mainProgram = "celine";
    license = lib.licenses.asl20;
    description = "Declarative Calibre and Kavita deployment over Tailscale";
  };
}
