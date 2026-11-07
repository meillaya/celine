{
  # Evaluation-only fixture for the Celine NixOS module. This system is never
  # built or deployed; it only lets `nix flake check` exercise the module.
  nixpkgs.hostPlatform = "x86_64-linux";

  system.stateVersion = "25.11";

  boot.loader.grub.enable = false;

  fileSystems."/" = {
    device = "/dev/vda1";
    fsType = "ext4";
  };

  networking.hostName = "celine-test";

  services.tailscale.enable = true;

  services.celine = {
    enable = true;
    root = ../.;
    stateDir = "/var/lib/celine";
    mediaRoot = "/srv/celine";
  };
}
