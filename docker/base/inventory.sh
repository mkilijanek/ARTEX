#!/bin/sh
# Prints the inventory of a tools-base image: what is installed and the hashes of the tool
# binaries, so two builds can be compared with diff. Read-only; run it as root inside the image:
#
#   docker run --rm -i --entrypoint sh <image> -s < docker/base/inventory.sh > inventory.txt
#
# pip freeze reads the bbot venv through the image Python's pip (--path), because the venv's
# own pip is removed from the image.
set -eu
export LC_ALL=C

echo "## dpkg (package version)"
dpkg-query -W -f='${Package} ${Version}\n' | sort

echo "## pip freeze (/opt/bbot)"
/usr/local/bin/python -m pip freeze --path /opt/bbot/lib/python3.12/site-packages | sort

echo "## npm ls -g --depth=0"
npm ls -g --depth=0

echo "## sha256 /usr/local/bin"
find /usr/local/bin -type f -o -type l | sort | while IFS= read -r f; do
  if [ -L "$f" ]; then
    echo "link $f -> $(readlink "$f")"
  else
    sha256sum "$f"
  fi
done

echo "## sha256 /opt/bbot-home/.bbot/tools (regular files)"
find /opt/bbot-home/.bbot/tools -type f | sort | xargs sha256sum

echo "## tools symlinks"
find /opt/bbot-home/.bbot/tools -type l | sort | while IFS= read -r f; do
  echo "link $f -> $(readlink "$f")"
done

echo "## setuid/setgid files"
find / -xdev -perm /6000 -type f | sort
