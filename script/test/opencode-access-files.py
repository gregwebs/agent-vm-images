#!/usr/bin/env python3
"""Independent inventory of physical content and lexical dependency-link paths."""
import os
from pathlib import Path
import sys


def content_files(prefix):
    prefix = prefix.resolve(strict=True)

    def walk(file, ancestors):
        real = file.resolve(strict=True)
        if not real.is_relative_to(prefix):
            raise ValueError(f"dependency escapes owned prefix: {file}")
        if file.is_file():
            yield file
        elif file.is_dir():
            if real in ancestors:
                raise ValueError(f"directory cycle: {file}")
            for child in file.iterdir():
                yield from walk(child, ancestors | {real})
        else:
            raise ValueError(f"unsupported payload file: {file}")

    yield from walk(prefix, set())


if __name__ == "__main__":
    try:
        for file in content_files(Path(sys.argv[1])):
            sys.stdout.buffer.write(os.fsencode(file) + b'\0')
    except (OSError, ValueError, RuntimeError) as error:
        sys.exit(f"OpenCode access inventory: {error}")
