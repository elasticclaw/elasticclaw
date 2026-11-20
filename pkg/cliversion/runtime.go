package cliversion

import "fmt"

const (
	NixInstallerVersion      = "3.24.2"
	NixInstallerAMD64SHA256  = "22ed14ee1b24d100d388f1fc245e3062bd5982433d159cffa6dec34c0eb15dab"
	NixInstallerARM64SHA256  = "bc18d8d8ca12f95d219b5978201e15d0049b2e9d8b7c49593f26503c87172145"
	NodeSourceKeyFingerprint = "6F71F525282841EEDAF851B42F59B5F99B1BE0B4"
	DockerKeyFingerprint     = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
	CaddyKeyFingerprint      = "65760C51EDEA2017CEA2CA15155B6D79CA56EA34"
	SmitheryCLIVersion       = "4.11.1"
)

// NixInstallerScript runs the verified installer with the caller's NIX_INIT_MODE.
const NixInstallerScript = `(
set -eu
case "$(uname -m)" in
  x86_64) NIX_ARCH=x86_64-linux; NIX_SHA256=` + NixInstallerAMD64SHA256 + ` ;;
  aarch64|arm64) NIX_ARCH=aarch64-linux; NIX_SHA256=` + NixInstallerARM64SHA256 + ` ;;
  *) echo "unsupported Nix installer architecture" >&2; exit 1 ;;
esac
NIX_INSTALLER=$(mktemp)
trap 'rm -f "$NIX_INSTALLER"' EXIT
curl -fsSL --proto '=https' "https://github.com/DeterminateSystems/nix-installer/releases/download/v` + NixInstallerVersion + `/nix-installer-$NIX_ARCH" -o "$NIX_INSTALLER"
printf '%s  %s\n' "$NIX_SHA256" "$NIX_INSTALLER" | sha256sum -c -
chmod +x "$NIX_INSTALLER"
"$NIX_INSTALLER" install linux --no-confirm --init "$NIX_INIT_MODE"
) >> /tmp/nix-install.log 2>&1
`

// VerifiedAPTKeyScript checks that the bundle holds exactly one primary key with
// the expected fingerprint before installing a keyring.
// Arguments are trusted script constants; sudo is either empty or "sudo ".
func VerifiedAPTKeyScript(url, keyring, fingerprint, sudo string) string {
	return fmt.Sprintf(`(
set -eu
APT_KEY=$(mktemp)
trap 'rm -f "$APT_KEY"' EXIT
curl -fsSL --proto '=https' "%s" -o "$APT_KEY"
KEY_INFO=$(gpg --batch --show-keys --with-colons "$APT_KEY")
[ "$(printf '%%s\n' "$KEY_INFO" | grep -c '^pub:')" = 1 ] || { echo "APT signing key bundle must hold exactly one primary key" >&2; exit 1; }
printf '%%s\n' "$KEY_INFO" | awk -F: '$1 == "pub" { primary=1; next } $1 == "sub" { primary=0 } $1 == "fpr" && primary { print; primary=0 }' | grep -Fx 'fpr:::::::::%s:' >/dev/null || { echo "APT signing key fingerprint mismatch" >&2; exit 1; }
%sgpg --batch --yes --dearmor -o %s "$APT_KEY"
)
`, url, fingerprint, sudo, keyring)
}
