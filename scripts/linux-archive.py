"""Build a gzip archive with Linux permissions from a staged Windows directory."""

import gzip
from pathlib import Path
import sys
import tarfile


def main():
    stage = Path(sys.argv[1]).resolve()
    destination = Path(sys.argv[2]).resolve()
    epoch = int(sys.argv[3])

    def normalize(item):
        item.uid = item.gid = 0
        item.uname = item.gname = ""
        item.mtime = epoch
        executable = item.isfile() and item.name == f"{stage.name}/upgrade-agent"
        item.mode = 0o755 if executable or item.isdir() else 0o644
        return item

    with destination.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=epoch) as stream:
            with tarfile.open(fileobj=stream, mode="w") as archive:
                archive.add(stage, arcname=stage.name, filter=normalize)


if __name__ == "__main__":
    main()
