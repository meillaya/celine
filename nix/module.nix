{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.celine;

  # Parse the deployment file at evaluation time so user-facing defaults can be
  # derived from it.
  c = builtins.fromTOML (builtins.readFile cfg.configFile);

  mediaRoot =
    if cfg.mediaRoot != null then
      cfg.mediaRoot
    else if lib.hasPrefix "/" c.paths.media_root then
      c.paths.media_root
    else
      throw "services.celine.mediaRoot must be set when celine.toml media_root is relative";

  # The token key comes either from a user-supplied file or from SecretSpec.
  materializeToken = c.kavita.enable && cfg.secretspec.enable && cfg.kavitaTokenKeyFile == null;

  tokenPath =
    if cfg.kavitaTokenKeyFile != null then
      cfg.kavitaTokenKeyFile
    else
      "${cfg.stateDir}/secrets/kavita-token";

  secretArgs =
    " --profile ${shellQuote cfg.secretspec.profile}"
    + lib.optionalString (cfg.secretspec.provider != null) " --provider ${shellQuote cfg.secretspec.provider}";

  secretEnv =
    lib.optionalAttrs (cfg.secretspec.sopsAgeKeyFile != null) {
      SOPS_AGE_KEY_FILE = cfg.secretspec.sopsAgeKeyFile;
    }
    // lib.optionalAttrs (cfg.secretspec.sopsAgeSshPrivateKeyFile != null) {
      SOPS_AGE_SSH_PRIVATE_KEY_FILE = cfg.secretspec.sopsAgeSshPrivateKeyFile;
    };

  secretWantsAfter = lib.optionals materializeToken [ "celine-secrets.service" ];

  # Quote user-controlled paths and values that are interpolated into shell
  # scripts inside the generated units.
  shellQuote = value: lib.escapeShellArg value;
in
{
  options.services.celine = {
    enable = lib.mkEnableOption "Celine, a declarative Calibre and Kavita deployment";

    package = lib.mkPackageOption pkgs "celine" { };

    root = lib.mkOption {
      type = lib.types.path;
      default = ./..;
      defaultText = lib.literalExpression "./..";
      description = ''
        Source tree containing {file}`celine.toml` and {file}`secretspec.toml`.
        Defaults to the Celine flake root.
      '';
    };

    configFile = lib.mkOption {
      type = lib.types.path;
      default = cfg.root + "/celine.toml";
      defaultText = lib.literalExpression "config.services.celine.root + \"/celine.toml\"";
      description = "Deployment configuration read by the celine CLI.";
    };

    stateDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/celine";
      description = "Directory holding Celine state, Kavita data, and materialized secrets.";
    };

    mediaRoot = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = ''
        Absolute path holding the Calibre and Kavita libraries. When null, the
        absolute `media_root` from {file}`celine.toml` is used; a relative value
        requires setting this option.
      '';
    };

    kavitaTokenKeyFile = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = ''
        File containing the Kavita token key. When set it is used directly and
        the SecretSpec materialization unit is not created.
      '';
    };

    backup = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = c.backup.enable;
        defaultText = lib.literalExpression "celine.toml [backup].enable";
        description = "Enable the scheduled Celine backup timer; defaults to [backup].enable.";
      };

      dir = lib.mkOption {
        type = lib.types.str;
        default = "${cfg.stateDir}/backups";
        defaultText = lib.literalExpression "config.services.celine.stateDir + \"/backups\"";
        description = "Directory that receives backup archives.";
      };

      retention = lib.mkOption {
        type = lib.types.int;
        default = c.backup.retention;
        defaultText = lib.literalExpression "celine.toml [backup].retention";
        description = "Number of backup archives to keep; defaults to [backup].retention.";
      };

      onCalendar = lib.mkOption {
        type = lib.types.str;
        default = c.backup.on_calendar;
        defaultText = lib.literalExpression "celine.toml [backup].on_calendar";
        description = "systemd OnCalendar expression for the backup timer; defaults to [backup].on_calendar.";
      };
    };

    tailnet = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Apply the Tailscale Serve configuration declared in celine.toml.";
      };
    };

    secretspec = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Materialize the Kavita token key through SecretSpec.";
      };

      package = lib.mkPackageOption pkgs "secretspec" { };

      manifest = lib.mkOption {
        type = lib.types.path;
        default = cfg.root + "/secretspec.toml";
        defaultText = lib.literalExpression "config.services.celine.root + \"/secretspec.toml\"";
        description = "SecretSpec manifest used by the celine CLI.";
      };

      profile = lib.mkOption {
        type = lib.types.str;
        default = "default";
        description = "SecretSpec profile to resolve.";
      };

      provider = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Optional SecretSpec provider override.";
      };

      sopsAgeKeyFile = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Optional SOPS age identity file for SecretSpec.";
      };

      sopsAgeSshPrivateKeyFile = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Optional SOPS age SSH identity file for SecretSpec.";
      };
    };
  };

  config = lib.mkIf cfg.enable (
    lib.mkMerge [
      {
        assertions = [
          {
            assertion = !c.kavita.enable || cfg.secretspec.enable || cfg.kavitaTokenKeyFile != null;
            message = "services.celine.kavitaTokenKeyFile must be set when services.celine.secretspec.enable is false";
          }
          {
            assertion = !materializeToken || cfg.secretspec.sopsAgeKeyFile != null || cfg.secretspec.sopsAgeSshPrivateKeyFile != null;
            message = "set services.celine.secretspec.sopsAgeKeyFile (or sopsAgeSshPrivateKeyFile) when materializing the Kavita token through SOPS; the unit runs outside your login session and cannot use an interactive default identity";
          }
          {
            assertion = lib.hasPrefix "/" cfg.stateDir;
            message = "services.celine.stateDir must be an absolute path";
          }
          {
            assertion = cfg.mediaRoot == null || lib.hasPrefix "/" cfg.mediaRoot;
            message = "services.celine.mediaRoot must be null or an absolute path";
          }
        ];

        services.calibre-server.enable = c.calibre.enable;
        services.kavita.enable = c.kavita.enable;

        # Upstream services.kavita creates this user; without it, calibre-server
        # still needs a user to run as.
        users.groups.kavita = lib.mkIf (!c.kavita.enable) { };
        users.users.kavita = lib.mkIf (!c.kavita.enable) {
          isSystemUser = true;
          group = "kavita";
          home = "${cfg.stateDir}/kavita";
        };

        systemd.tmpfiles.rules = [
          "d ${cfg.stateDir} 0700 kavita kavita -"
          "d ${cfg.stateDir}/secrets 0700 kavita kavita -"
        ];

        # One target anchors every boot unit. Upstream services already carry
        # their own wantedBy entries; this target makes the complete boot set
        # explicit and gives operators `systemctl status celine.target`.
        systemd.targets.celine = {
          description = "Celine Calibre and Kavita stack";
          wantedBy = [ "multi-user.target" ];
          wants =
            lib.optionals c.calibre.enable [ "calibre-server.service" ]
            ++ lib.optionals c.kavita.enable [ "kavita.service" ]
            ++ lib.optionals materializeToken [ "celine-secrets.service" ]
            ++ lib.optionals (cfg.tailnet.enable && c.tailnet.enable) [ "celine-tailnet.service" ]
            ++ lib.optionals cfg.backup.enable [ "celine-backup.timer" ];
        };
      }

      (lib.mkIf c.calibre.enable {
        services.calibre-server = {
          package = pkgs.calibre;
          libraries = map (l: mediaRoot + "/" + l.path) c.calibre.libraries;
          host = "127.0.0.1";
          port = c.calibre.port;
          auth.enable = false;
          user = "kavita";
          group = "kavita";
          openFirewall = false;
        };
      })

      (lib.mkIf c.kavita.enable {
        services.kavita = {
          user = "kavita";
          dataDir = "${cfg.stateDir}/kavita";
          # Current nixpkgs injects secrets through `credentials`; the token key
          # file path replaces the "@token_key@" placeholder at service startup.
          credentials.token_key = tokenPath;
          settings = {
            Port = c.kavita.port;
            IpAddresses = "127.0.0.1";
            BaseUrl = c.kavita.base_url;
            Cache = c.kavita.cache;
            AllowIFraming = c.kavita.allow_iframing;
          };
        };

        systemd.services.kavita = {
          wants = secretWantsAfter;
          requires = secretWantsAfter;
          after = secretWantsAfter;
          serviceConfig.BindPaths = [
            "${mediaRoot}/${c.kavita.library_subdir}:${c.kavita.library_mount}"
            "${cfg.stateDir}/kavita/config:/app/kavita/config"
          ];
        };
      })

      (lib.mkIf materializeToken {
        systemd.services.celine-secrets = {
          description = "Materialize Celine secrets for Kavita";
          wantedBy = [ "multi-user.target" ];
          before = [ "kavita.service" ];
          path = [
            cfg.package
            cfg.secretspec.package
            pkgs.sops
            pkgs.sqlite
          ];
          environment = secretEnv;
          serviceConfig = {
            Type = "oneshot";
            RemainAfterExit = true;
            UMask = "0077";
          };
          unitConfig.RequiresMountsFor = [ cfg.stateDir ];
          script = ''
            install -d -m 0700 ${shellQuote (cfg.stateDir + "/secrets")}
            ${cfg.package}/bin/celine secrets materialize --config ${cfg.configFile} --secretspec ${cfg.secretspec.manifest} --name KAVITA_TOKEN_KEY --out ${shellQuote tokenPath}${secretArgs}
          '';
        };
      })

      (lib.mkIf (cfg.tailnet.enable && c.tailnet.enable) {
        systemd.services.celine-tailnet = {
          description = "Apply the Celine Tailscale Serve configuration";
          wantedBy = [ "multi-user.target" ];
          after = [
            "tailscaled.service"
            "network-online.target"
          ];
          wants = [
            "tailscaled.service"
            "network-online.target"
          ];
          path = [
            cfg.package
            pkgs.tailscale
          ];
          serviceConfig = {
            Type = "oneshot";
            RemainAfterExit = true;
            Restart = "on-failure";
            RestartSec = "30s";
          };
          unitConfig = {
            StartLimitIntervalSec = 300;
            StartLimitBurst = 10;
          };
          script = "${cfg.package}/bin/celine tailnet apply --config ${cfg.configFile}";
        };
      })

      (lib.mkIf cfg.backup.enable {
        systemd.services.celine-backup = {
          description = "Back up Celine state";
          serviceConfig.Type = "oneshot";
          unitConfig.RequiresMountsFor = [ cfg.stateDir cfg.backup.dir ];
          path = [
            cfg.package
            pkgs.sqlite
            pkgs.sops
            pkgs.secretspec
          ];
          script = "${cfg.package}/bin/celine backup --config ${cfg.configFile} --state-dir ${shellQuote cfg.stateDir} --backup-dir ${shellQuote cfg.backup.dir} --retention ${toString cfg.backup.retention}";
        };

        systemd.timers.celine-backup = {
          description = "Scheduled Celine backups";
          wantedBy = [ "timers.target" ];
          timerConfig = {
            OnCalendar = cfg.backup.onCalendar;
            Persistent = true;
          };
        };
      })
    ]
  );

  meta.maintainers = [ ];
}
