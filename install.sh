#!/bin/sh
# Установка orchestra-tennant из GitHub Releases: без Go, без клона.
#
#   curl -fsSL https://raw.githubusercontent.com/realkasparov/orchestra-tennant/main/install.sh | sh
#
# Переменные: ORCHESTRA_TENNANT_VERSION (тег, по умолчанию последний релиз),
#             ORCHESTRA_TENNANT_BIN (папка установки, по умолчанию ~/.local/bin).
set -eu

repo="realkasparov/orchestra-tennant"
bin_dir="${ORCHESTRA_TENNANT_BIN:-$HOME/.local/bin}"
version="${ORCHESTRA_TENNANT_VERSION:-}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$os" in
  darwin|linux) ;;
  *) echo "orchestra-tennant: неподдерживаемая ОС: $os (нужны macOS или Linux)" >&2; exit 1 ;;
esac
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "orchestra-tennant: неподдерживаемая архитектура: $arch" >&2; exit 1 ;;
esac

if [ -z "$version" ]; then
  # Последний релиз — по редиректу GitHub, без API и без токена.
  version=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" | sed 's|.*/tag/||')
  [ -n "$version" ] || { echo "orchestra-tennant: не удалось узнать последний релиз" >&2; exit 1; }
fi

name="orchestra-tennant_${os}_${arch}"
url="https://github.com/$repo/releases/download/$version/$name.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Скачиваю $version для $os/$arch…"
curl -fsSL "$url" -o "$tmp/$name.tar.gz"
curl -fsSL "https://github.com/$repo/releases/download/$version/checksums.txt" -o "$tmp/checksums.txt"
# Контрольная сумма: sha256sum на Linux, shasum на macOS.
expected=$(grep " $name.tar.gz\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$name.tar.gz" | cut -d' ' -f1)
else
  actual=$(shasum -a 256 "$tmp/$name.tar.gz" | cut -d' ' -f1)
fi
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
  echo "orchestra-tennant: контрольная сумма не сошлась — архив повреждён или подменён" >&2
  exit 1
fi
tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

mkdir -p "$bin_dir"
install -m 0755 "$tmp/orchestra-tennant" "$bin_dir/orchestra-tennant"
echo "Установлено: $bin_dir/orchestra-tennant ($("$bin_dir/orchestra-tennant" version))"

case ":$PATH:" in
  *":$bin_dir:"*) ;;
  *) echo
     echo "Папки $bin_dir нет в PATH. Добавьте в профиль оболочки:"
     echo "  export PATH=\"$bin_dir:\$PATH\"" ;;
esac
echo
echo "Дальше: orchestra-tennant setup -orchestrator <адрес> -pair-key <ключ из панели «Добавить устройство»>"
