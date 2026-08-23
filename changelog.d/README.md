# Pending changelog entries

One file per user-visible change. `CHANGELOG.md` is assembled from these files
at release time and is not edited by feature branches, so two pull requests
that both change user-visible behavior no longer write to the same lines of the
same file and have nothing to conflict over.

Name the file `<slug>.<category>.md`, where the slug is lowercase and
hyphenated and the category is one of `added`, `changed`, `deprecated`,
`removed`, `fixed`, or `security`:

```sh
./scripts/changelog.py new fixed provider-unit-bind-capability
```

Write the entry as the released changelog should read it: prose, wrapped at 78
columns, with no leading `- ` — the release adds the list marker and the
two-column continuation indent. Say what changed and why it mattered; an entry
that only names a symbol is not worth the file. Fragments are grouped by
category and sorted by slug, so the order within a category is not the order
they merged and is not a place to encode importance.

A change with no user-visible effect needs no file. Refactors, test-only
changes, and internal documentation are the normal cases.

```sh
./scripts/changelog.py check      # what CI runs on every pull request
./scripts/changelog.py preview    # the section these files will produce
```

Releases run `./scripts/changelog.py release --version vX.Y.Z`, which inserts
the assembled section above the newest released one and deletes every file
here. See [`docs/RELEASING.md`](../docs/RELEASING.md).
