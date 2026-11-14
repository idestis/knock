#!/usr/bin/env python3
"""tap-update.py <version> <checksums.txt> <tap-dir>

Points the Homebrew tap (github.com/idestis/homebrew-tap) at one release:
rewrites `version "..."`, the version in each url, and every `sha256 "..."` in Casks/knock.rb and
Formula/knock.rb. Each sha256 belongs to the nearest `url` at the same
indentation (above it in the formula, below it in the cask); the url's file
name, with #{version} filled in, is looked up in checksums.txt.
"""
import pathlib
import re
import sys

FILES = ("Formula/knock.rb", "Casks/knock.rb")
URL = re.compile(r'^(\s*)url "([^"]+)"')
SHA = re.compile(r'^(\s*)sha256 "[^"]*"')
VERSION = re.compile(r'^(\s*)version "[^"]*"')


def main(version, sums_path, tap):
    if not re.fullmatch(r"\d+\.\d+\.\d+", version):
        sys.exit(f"tap-update: bad version {version!r}")
    sums = {}
    for line in pathlib.Path(sums_path).read_text().splitlines():
        parts = line.split()
        if len(parts) == 2:
            sums[parts[1].lstrip("*")] = parts[0].lower()

    updated = {}
    for rel in FILES:
        path = pathlib.Path(tap, rel)
        text = path.read_text()
        # The formula spells the version out in each url (brew audit wants that); the cask uses #{version}.
        old = re.search(r"/download/v(\d+\.\d+\.\d+)/", text)
        lines = text.splitlines(keepends=True)
        if old:
            lines = [l.replace(old.group(1), version) if URL.match(l) else l for l in lines]
        urls = [(i, m.group(1), m.group(2)) for i, l in enumerate(lines) if (m := URL.match(l))]
        out = []
        for i, line in enumerate(lines):
            if m := VERSION.match(line):
                line = f'{m.group(1)}version "{version}"\n'
            elif m := SHA.match(line):
                indent = m.group(1)
                near = [u for u in urls if u[1] == indent]
                if not near:
                    sys.exit(f"tap-update: {rel}:{i + 1} has a sha256 with no url next to it")
                url = min(near, key=lambda u: abs(u[0] - i))[2]
                name = url.rsplit("/", 1)[-1].replace("#{version}", version)
                if name not in sums:
                    sys.exit(f"tap-update: {sums_path} doesn't list {name}")
                line = f'{indent}sha256 "{sums[name]}"\n'
            out.append(line)
        updated[path] = "".join(out)

    # Write only once every file resolved, so a missing checksum leaves the tap untouched.
    for path, text in updated.items():
        path.write_text(text)
        print(f"tap-update: {path.relative_to(tap)} -> {version}")


if __name__ == "__main__":
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    main(*sys.argv[1:])
