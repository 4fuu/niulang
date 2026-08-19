"""Measurement and release-gate harnesses.

This file exists so that `scripts` is a regular package rather than a
namespace package. The test modules here import their subjects as
`from scripts import <module>`, and a namespace package loses that name to
any installed distribution also called `scripts`, no matter where the
repository sits on `sys.path`: a regular package anywhere beats a namespace
portion everywhere. The tests then fail to import rather than fail to pass,
so a clean continuous-integration runner reports success while a developer
machine with that distribution installed silently skips them.
"""
