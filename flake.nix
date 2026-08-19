{
  description = "SkillLoop Go development environment";

  inputs = {
    nixpkgs.url = "https://flakehub.com/f/NixOS/nixpkgs/0.2605";
  };

  outputs = { nixpkgs, ... }:
    let
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];

      forEachSupportedSystem = f:
        nixpkgs.lib.genAttrs supportedSystems (system:
          let
            pkgs = import nixpkgs { inherit system; };
          in
          f { inherit pkgs; });
    in
    {
      devShells = forEachSupportedSystem ({ pkgs }:
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go_1_26
              pkgs.actionlint
              pkgs.git
              pkgs.golangci-lint
              pkgs.goreleaser
              pkgs.govulncheck
              pkgs.jq
              pkgs.just
              pkgs.sqlite
            ];
          };
        });
    };
}
