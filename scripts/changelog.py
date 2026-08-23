#!/usr/bin/env python3
"""Assemble CHANGELOG.md from one file per change, so branches cannot collide."""

from __future__ import annotations

import argparse
import datetime
import pathlib
import re
import sys
import textwrap


# Keep a Changelog's order, which is the order the released sections already
# use. A change names its category in its filename, so two pull requests never
# write to the same file and a merge has nothing to resolve.
CATEGORIES = ("added", "changed", "deprecated", "removed", "fixed", "security")
HEADINGS = {category: category.capitalize() for category in CATEGORIES}

FRAGMENT_NAME = re.compile(
    r"\A(?P<slug>[a-z0-9]+(?:-[a-z0-9]+)*)\.(?P<category>[a-z]+)\.md\Z"
)
VERSION_HEADING = re.compile(
    r"\A## (?P<version>v\S+) - (?P<date>\d{4}-\d{2}-\d{2})\s*\Z"
)
VERSION = re.compile(r"\Av(?P<release>\d+\.\d+\.\d+)(?:-[0-9A-Za-z.]+)?\Z")
FENCE = re.compile(r"\A\s*(?:```|~~~)")

# The released sections wrap prose; a rendered bullet adds "- " or "  " to
# every line, so a fragment is authored two columns narrower than the file.
WIDTH = 80
WRAP = WIDTH - 2


class Fragment:
    def __init__(self, path: pathlib.Path, slug: str, category: str, body: str):
        self.path = path
        self.slug = slug
        self.category = category
        self.body = body

    def bullet(self) -> list[str]:
        lines = self.body.split("\n")
        rendered = ["- " + lines[0]]
        for line in lines[1:]:
            rendered.append("  " + line if line else "")
        return rendered


def overlong(lines: list[str]):
    """Yield rendered lines past the column limit, ignoring fenced blocks."""
    fenced = False
    for number, line in enumerate(lines, start=1):
        if FENCE.match(line):
            fenced = not fenced
            continue
        if not fenced and len(line) > WIDTH:
            yield number, line


def read_fragments(directory: pathlib.Path) -> tuple[list[Fragment], list[str]]:
    fragments: list[Fragment] = []
    problems: list[str] = []
    if not directory.is_dir():
        return fragments, [f"{directory}: fragment directory is missing"]
    for path in sorted(directory.iterdir()):
        if path.name.startswith(".") or path.name == "README.md":
            continue
        if not path.is_file():
            problems.append(f"{path}: not a file")
            continue
        match = FRAGMENT_NAME.match(path.name)
        if match is None:
            problems.append(
                f"{path}: name a fragment <slug>.<category>.md, with a "
                "lowercase hyphenated slug"
            )
            continue
        category = match.group("category")
        if category not in CATEGORIES:
            problems.append(
                f"{path}: unknown category {category!r}; use one of "
                + ", ".join(CATEGORIES)
            )
            continue
        raw = path.read_bytes()
        if b"\r" in raw:
            problems.append(f"{path}: use LF line endings")
            continue
        try:
            text = raw.decode("utf-8")
        except UnicodeDecodeError as error:
            problems.append(f"{path}: not UTF-8: {error}")
            continue
        body = text.strip("\n")
        if not body.strip():
            problems.append(f"{path}: empty; write the entry as it should read")
            continue
        if not text.endswith("\n"):
            problems.append(f"{path}: end the file with a newline")
            continue
        if re.match(r"\A\s*[-*+] ", body):
            problems.append(
                f"{path}: write the entry without a list marker; the release "
                "adds it"
            )
            continue
        fragment = Fragment(path, match.group("slug"), category, body)
        for number, line in overlong(fragment.bullet()):
            problems.append(
                f"{path}:{number}: {len(line)} columns once rendered; wrap "
                f"fragment prose at {WRAP}"
            )
        fragments.append(fragment)
    return fragments, problems


def check_changelog(path: pathlib.Path) -> list[str]:
    """Check the released file itself: unreleased work belongs in fragments."""
    problems: list[str] = []
    if not path.is_file():
        return [f"{path}: missing"]
    lines = path.read_text(encoding="utf-8").split("\n")
    if not lines or lines[0] != "# Changelog":
        problems.append(f"{path}:1: expected a '# Changelog' heading")
    previous: tuple[int, ...] | None = None
    seen: set[str] = set()
    for number, line in enumerate(lines, start=1):
        if line.startswith("## ") and re.match(r"\A## unreleased\b", line, re.I):
            problems.append(
                f"{path}:{number}: no Unreleased section; pending changes are "
                "files in changelog.d/"
            )
            continue
        match = VERSION_HEADING.match(line)
        if match is None:
            continue
        version, date = match.group("version"), match.group("date")
        try:
            datetime.date.fromisoformat(date)
        except ValueError:
            problems.append(f"{path}:{number}: {date} is not a calendar date")
        if version in seen:
            problems.append(f"{path}:{number}: {version} appears twice")
        seen.add(version)
        release = VERSION.match(version)
        if release is None:
            problems.append(f"{path}:{number}: {version} is not vMAJOR.MINOR.PATCH")
            continue
        ordered = tuple(int(part) for part in release.group("release").split("."))
        if previous is not None and ordered >= previous:
            problems.append(
                f"{path}:{number}: {version} is not below the section above it"
            )
        previous = ordered
    return problems


def render_section(heading: str, fragments: list[Fragment]) -> list[str]:
    lines = [heading, ""]
    for category in CATEGORIES:
        chosen = sorted(
            (f for f in fragments if f.category == category), key=lambda f: f.slug
        )
        if not chosen:
            continue
        lines.append(f"### {HEADINGS[category]}")
        lines.append("")
        for fragment in chosen:
            lines.extend(fragment.bullet())
        lines.append("")
    return lines


def insert_section(text: str, section: list[str]) -> str:
    """Insert above the newest released section, leaving released bytes alone."""
    lines = text.split("\n")
    index = next(
        (i for i, line in enumerate(lines) if VERSION_HEADING.match(line)), None
    )
    if index is None:
        index = next(
            (i for i, line in enumerate(lines) if line.startswith("## ")), len(lines)
        )
    return "\n".join(lines[:index] + section + lines[index:])


def report(problems: list[str]) -> int:
    for problem in problems:
        print(problem, file=sys.stderr)
    if problems:
        print(f"{len(problems)} changelog problem(s)", file=sys.stderr)
        return 1
    return 0


def command_check(args: argparse.Namespace) -> int:
    fragments, problems = read_fragments(args.root / "changelog.d")
    problems += check_changelog(args.root / "CHANGELOG.md")
    if report(problems):
        return 1
    print(f"{len(fragments)} pending change(s), CHANGELOG.md well formed")
    return 0


def command_preview(args: argparse.Namespace) -> int:
    fragments, problems = read_fragments(args.root / "changelog.d")
    if report(problems):
        return 1
    if not fragments:
        print("no pending changes", file=sys.stderr)
        return 0
    heading = "## Unreleased"
    if args.version:
        heading = f"## {args.version} - {args.date or today()}"
    print("\n".join(render_section(heading, fragments)).rstrip())
    return 0


def command_release(args: argparse.Namespace) -> int:
    changelog = args.root / "CHANGELOG.md"
    fragments, problems = read_fragments(args.root / "changelog.d")
    problems += check_changelog(changelog)
    if VERSION.match(args.version) is None:
        problems.append(f"{args.version} is not vMAJOR.MINOR.PATCH")
    date = args.date or today()
    try:
        datetime.date.fromisoformat(date)
    except ValueError:
        problems.append(f"{date} is not a calendar date")
    if not fragments and not args.allow_empty:
        problems.append(
            "no pending changes in changelog.d/; --allow-empty releases anyway"
        )
    if report(problems):
        return 1
    text = changelog.read_text(encoding="utf-8")
    for line in text.split("\n"):
        match = VERSION_HEADING.match(line)
        if match and match.group("version") == args.version:
            return report([f"{changelog}: {args.version} is already released"])
    section = render_section(f"## {args.version} - {date}", fragments)
    changelog.write_text(insert_section(text, section), encoding="utf-8")
    for fragment in fragments:
        fragment.path.unlink()
    print(f"{changelog}: added {args.version} - {date}")
    for fragment in sorted(fragments, key=lambda f: (f.category, f.slug)):
        print(f"  {fragment.category}: {fragment.slug}")
    return 0


def command_new(args: argparse.Namespace) -> int:
    problems: list[str] = []
    if args.category not in CATEGORIES:
        problems.append(
            f"unknown category {args.category!r}; use one of " + ", ".join(CATEGORIES)
        )
    if FRAGMENT_NAME.match(f"{args.slug}.fixed.md") is None:
        problems.append(f"{args.slug!r} is not a lowercase hyphenated slug")
    if report(problems):
        return 1
    path = args.root / "changelog.d" / f"{args.slug}.{args.category}.md"
    if path.exists():
        return report([f"{path}: already exists"])
    text = args.message
    if text is None and not sys.stdin.isatty():
        text = sys.stdin.read()
    body = wrap(text) if text and text.strip() else ""
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body + "\n" if body else "", encoding="utf-8")
    print(path)
    if not body:
        print("write the entry as one bullet of prose", file=sys.stderr)
    return 0


def wrap(text: str) -> str:
    """Rewrap prose to the fragment width; leave anything fenced untouched."""
    stripped = text.strip("\n").rstrip()
    if "```" in stripped or "~~~" in stripped:
        return stripped
    paragraphs = re.split(r"\n\s*\n", stripped)
    return "\n\n".join(
        textwrap.fill(" ".join(paragraph.split()), width=WRAP)
        for paragraph in paragraphs
        if paragraph.strip()
    )


def today() -> str:
    return datetime.datetime.now(datetime.timezone.utc).date().isoformat()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--root",
        type=pathlib.Path,
        default=pathlib.Path(__file__).resolve().parent.parent,
        help="repository root holding CHANGELOG.md and changelog.d/",
    )
    commands = parser.add_subparsers(dest="command", required=True)

    check = commands.add_parser("check", help="validate pending fragments and the file")
    check.set_defaults(handler=command_check)

    preview = commands.add_parser("preview", help="print the pending section")
    preview.add_argument("--version", help="render as this release instead of Unreleased")
    preview.add_argument("--date", help="release date, default today in UTC")
    preview.set_defaults(handler=command_preview)

    release = commands.add_parser("release", help="move fragments into CHANGELOG.md")
    release.add_argument("--version", required=True, help="release version, e.g. v0.1.2")
    release.add_argument("--date", help="release date, default today in UTC")
    release.add_argument(
        "--allow-empty", action="store_true", help="release with no pending changes"
    )
    release.set_defaults(handler=command_release)

    new = commands.add_parser("new", help="start a fragment for the current change")
    new.add_argument("category", help=", ".join(CATEGORIES))
    new.add_argument("slug", help="lowercase hyphenated name for the change")
    new.add_argument("-m", "--message", help="entry text; otherwise read from stdin")
    new.set_defaults(handler=command_new)

    args = parser.parse_args(argv)
    return args.handler(args)


if __name__ == "__main__":
    sys.exit(main())
