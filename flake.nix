{
  description = "Celine: declarative Calibre and Kavita deployment over Tailscale";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          celine = pkgs.callPackage ./nix/package.nix { };
        in
        {
          default = celine;
          celine = celine;
        }
      );

      overlays.default = final: _prev: {
        celine = final.callPackage ./nix/package.nix { };
      };

      # Inject the flake package as the default so consumers do not need the
      # overlay just to use the module.
      nixosModules.default =
        {
          lib,
          pkgs,
          ...
        }:
        {
          imports = [ ./nix/module.nix ];
          services.celine.package = lib.mkDefault self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        };

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt-rfc-style);

      checks = forAllSystems (system: {
        default = self.packages.${system}.default;
      });

      # Evaluation-only fixture; it is never built or deployed.
      nixosConfigurations.celine-test = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          self.nixosModules.default
          ./tests/nixos-test.nix
        ];
      };
    };
}
