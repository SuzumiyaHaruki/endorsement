#!/usr/bin/env bash
#set -e

#mkdir "$2"
#ln -sfT "$2" latest
#cd "$2"
#echo "$2" > module-root.txt
#url_base="https://github.com/OffchainLabs/nitro/releases/download/$1"
#wget "$url_base/machine.wavm.br"

#status_code="$(curl -LI "$url_base/replay.wasm" -so /dev/null -w '%{http_code}')"
#if [ "$status_code" -ne 404 ]; then
#	wget "$url_base/replay.wasm"
#fi

#!/usr/bin/env bash
set -e

version="$1"
module_root="$2"

mkdir -p "$module_root"
ln -sfT "$module_root" latest
cd "$module_root"
echo "$module_root" > module-root.txt

url_base="https://github.com/OffchainLabs/nitro/releases/download/$version"

rm -f machine.wavm.br replay.wasm

curl --retry 20 --retry-delay 5 --retry-all-errors -L -o machine.wavm.br \
  "$url_base/machine.wavm.br"

status_code="$(curl --retry 10 --retry-delay 3 --retry-all-errors -LI "$url_base/replay.wasm" -so /dev/null -w '%{http_code}')"
if [ "$status_code" != "404" ]; then
    curl --retry 20 --retry-delay 5 --retry-all-errors -L -o replay.wasm \
      "$url_base/replay.wasm"
fi
