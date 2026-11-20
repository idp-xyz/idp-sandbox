#!/usr/bin/env bash
# Build the office run image of idp-sandbox (ADR-0001 D14, D17): officecli, and what reads and writes PDF, each pinned
# by SHA-256, then docker build --network none, then a smoke run under the same restrictions as a sandbox run.
#
#   bash deploy/office/build.sh [TAG]       default idp-sandbox-office:<officecli version>
#   OFFICECLI_BINARY=file  a binary already downloaded (still checked against the pinned SHA-256)
#   IDP_SANDBOX_CACHE=dir  where downloads are kept (default ~/.cache/idp-sandbox); a file already there is used when
#                          it has the pinned SHA-256, so the build needs no network once the directory is filled
#   BASE=image             the base (default python:3.12-slim, the python run image; the wheels are for CPython 3.12
#                          on x86-64 glibc)
#
# Then point idp-sandbox at it: IDP_SANDBOX_OFFICE_IMAGE=<TAG>. officecli is github.com/iOfficeAI/OfficeCLI (Apache-2.0).
set -euo pipefail
cd "$(dirname "$0")/../.."
VERSION=1.0.159   # published 2026-10-27; move the pin deliberately, with the checksum of the new release
SHA256=92d9aeed324d91ed2a107f7f8dc7468829b14390a66cf2bbdfb25c40beac50ea   # officecli-linux-x64 of that release
# PDF and Markdown (ADR-0001 D17): SHA-256, where it goes in the image's build context, its name there, its source.
# The wheels' checksums are the ones PyPI publishes for these files. The fonts are Google Fonts' static TrueType files
# of Noto Sans SC 2.004 (SIL Open Font License 1.1, the text is OFL.txt); reportlab embeds TrueType outlines only,
# which is why they are not the OpenType files of the Noto CJK release. Move a pin deliberately, with its checksum.
PINS=(
  # pypdf 6.21.4 (BSD-3-Clause), published 2026-10-28: reads a PDF
  "33e4fb41ca352810eb4e9838aa0d82d6888a5f8bf682f43e99cce5faa666255c wheels pypdf-6.21.4-py3-none-any.whl https://files.pythonhosted.org/packages/3f/4b/77e8ad4ac42c9d93600f40541a50e9feb3a61d2bbee2d76f659346072051/pypdf-6.21.4-py3-none-any.whl"
  # pycryptodome 4.3.0 (BSD-2-Clause, Unlicense, MIT), published 2026-11-02: pypdf opens AES-encrypted files with it
  "96bf142ef25544a221a2b4a8f5d449cb07c1736be71b4ea057749318a2cd402c wheels pycryptodome-4.3.0-cp39-abi3-manylinux2014_x86_64.manylinux_2_17_x86_64.whl https://files.pythonhosted.org/packages/af/1b/b0f5f020bd43351932df9d16857330d3304715c4b1f7f783dc6e58cb7919/pycryptodome-4.3.0-cp39-abi3-manylinux2014_x86_64.manylinux_2_17_x86_64.whl"
  # reportlab 5.1.0 (BSD), published 2026-10-22: writes a PDF; it requires the two below
  "59fdbaec5efe437095ad1d7292ec9a271c8ca2d3ec75e889c6e61400929b90df wheels reportlab-5.1.0-py3-none-any.whl https://files.pythonhosted.org/packages/4a/3c/bf68d44f160b44685fc714f746a954c71d0091b3dc4fd3c8670154b44841/reportlab-5.1.0-py3-none-any.whl"
  # Pillow 13.0.0 (MIT-CMU), published 2026-10-16
  "42d63be3af4f9b951b199b0a9d23365d37ab14687e6d77b74940432c8515c4ca wheels pillow-13.0.0-cp312-cp312-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl https://files.pythonhosted.org/packages/e6/99/fdf65cc539e266ab93546c261bdf98411fe165c6e4e13f9dbf597097192b/pillow-13.0.0-cp312-cp312-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl"
  # charset-normalizer 3.5.2 (MIT), published 2026-09-30
  "b6b751274acb69d77b3323d6b7dbaa3c7fdfc1eb829b7eb61d262f32e1af9685 wheels charset_normalizer-3.5.2-py3-none-any.whl https://files.pythonhosted.org/packages/fc/ad/d07d7862a62ffa6d79d68074d14823243dd235a77c45262acbf6adeb28bf/charset_normalizer-3.5.2-py3-none-any.whl"
  "450625c8d46ab3df97b7904ded955ec2746d17ec76740cb1e91d1ba63a0f89af fonts NotoSansSC-Regular.ttf https://fonts.gstatic.com/s/notosanssc/v41/k3kCo84MPvpLmixcA63oeAL7Iqp5IZJF9bmaG9_FnYw.ttf"
  "0066a522a1ac007c1d72bc4fccb114f80ff7294641c78cead9715bd14d43b9ea fonts NotoSansSC-Bold.ttf https://fonts.gstatic.com/s/notosanssc/v41/k3kCo84MPvpLmixcA63oeAL7Iqp5IZJF9bmaGzjCnYw.ttf"
  "1c05c68c34f9708415aada51f17e1b0092d2cea709bf4a94cd38114f9e73d7d9 fonts OFL.txt https://raw.githubusercontent.com/google/fonts/2894aab31764f10f29c421bdfd2340d3b382d384/ofl/notosanssc/OFL.txt"
)
TAG="${1:-idp-sandbox-office:$VERSION}"
BASE="${BASE:-python:3.12-slim}"
CACHE="${IDP_SANDBOX_CACHE:-$HOME/.cache/idp-sandbox}"
has() { [[ -f "$2" ]] && echo "$1  $2" | sha256sum -c --quiet - 2>/dev/null; }   # has SHA256 FILE
fetch() { # fetch SHA256 FILE URL: afterwards FILE holds the pinned bytes; downloaded only when it does not yet
  has "$1" "$2" && return
  mkdir -p "$(dirname "$2")"
  local rc
  for _ in $(seq 1 30); do   # downloads can be slow through a proxy: resume, one HTTP/1.1 stream at a time
    has "$1" "$2.part" && break
    rc=0; curl -sSL --fail --http1.1 -C - -m 600 -o "$2.part" "$3" || rc=$?
    if (( rc == 0 )) && ! has "$1" "$2.part"; then   # complete, and not the pinned file: trying again cannot help
      rm -f "$2.part"; echo "office: $3 is not the pinned $(basename "$2"): its SHA-256 differs" >&2; exit 1
    fi
    (( rc != 22 && rc != 33 )) || rm -f "$2.part"   # the server refused or cannot resume from here: start over
    (( rc == 0 )) || sleep 5
  done
  has "$1" "$2.part" || { echo "office: download of $(basename "$2") failed (rerun to resume)" >&2; exit 1; }
  mv "$2.part" "$2"
}
BIN="${OFFICECLI_BINARY:-$CACHE/officecli-$VERSION-linux-x64}"
if [[ -n "${OFFICECLI_BINARY:-}" ]]; then
  has "$SHA256" "$BIN" || { echo "office: $BIN does not have the pinned SHA-256" >&2; exit 1; }
else
  fetch "$SHA256" "$BIN" "https://github.com/iOfficeAI/OfficeCLI/releases/download/v$VERSION/officecli-linux-x64"
fi
for pin in "${PINS[@]}"; do
  read -r sha _ file url <<< "$pin"
  fetch "$sha" "$CACHE/$file" "$url"
done
OUT=build/office
umask 022
rm -rf "$OUT" && mkdir -p "$OUT/wheels" "$OUT/fonts"
cp deploy/office/Dockerfile "$OUT/"
install -m 0755 "$BIN" "$OUT/officecli"
for tool in sheetskel sheetmap sheetcheck pdftext md2pdf; do install -m 0755 "deploy/office/$tool" "$OUT/$tool"; done
install -m 0644 deploy/office/sheetlib.py "$OUT/sheetlib.py"   # imported by sheetmap and sheetcheck from their own directory
for pin in "${PINS[@]}"; do
  read -r _ dir file _ <<< "$pin"
  install -m 0644 "$CACHE/$file" "$OUT/$dir/$file"
done
bash deploy/pull-image.sh "$BASE"   # with retries, so that docker build does not pull the base
DOCKER_BUILDKIT=1 docker build --network none -q --build-arg "BASE=$BASE" -t "$TAG" "$OUT" >/dev/null
smoke() { # a command in the image, under the restrictions of a sandbox run
  docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges --user 65534:65534 \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m -e HOME=/tmp "$TAG" "$@"
}
got="$(smoke officecli --version)"
[[ "$got" == "$VERSION" ]] || { echo "office: the image answers '$got', not $VERSION" >&2; exit 1; }
help="$(smoke sheetskel --help)"
[[ "$help" == *"FILE.xlsx"* ]] || { echo "office: sheetskel --help answered '$help'" >&2; exit 1; }
# a workbook is mapped and checked in one command each, with only the standard library
smap="$(smoke sh -c 'cd /tmp && printf "a" >/dev/null && sheetmap --help && sheetcheck --help')"
[[ "$smap" == *"sheetmap FILE.xlsx"* && "$smap" == *"sheetcheck OUT.xlsx"* ]] || { echo "office: sheetmap/sheetcheck --help answered '$smap'" >&2; exit 1; }
smoke sh -c 'cd /tmp && officecli create t.xlsx >/dev/null && sheetmap t.xlsx >/dev/null && sheetcheck t.xlsx | grep -q "VALIDATE"' ||
  { echo "office: sheetmap and sheetcheck did not run on a workbook officecli made" >&2; exit 1; }
# a Markdown file with Chinese and a table becomes a PDF and the PDF reads back: the wheels, the fonts and both tools
pdf="$(smoke sh -c 'cd /tmp && printf "# 报价单\n\n含税总价 **1,280.50** 元。\n\n| 渠道 | 价格 |\n|---|---:|\n| 空运 | 12.5 |\n" > q.md && md2pdf q.md q.pdf >/dev/null && pdftext q.pdf')"
[[ "$pdf" == "q.pdf: 1 page"* && "$pdf" == *"报价单"* && "$pdf" == *"含税总价"* && "$pdf" == *"1,280.50"* && "$pdf" == *"空运"* ]] ||
  { echo "office: the PDF md2pdf wrote did not read back with pdftext: '$pdf'" >&2; exit 1; }
echo "OK: $TAG $(docker image inspect --format '{{.Id}}' "$TAG" | cut -c1-19)… officecli $VERSION sha256 ${SHA256:0:12}…, ${#PINS[@]} pinned files for PDF"
