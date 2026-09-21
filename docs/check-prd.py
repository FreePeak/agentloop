#!/usr/bin/env python3
"""Structural check for docs/PRD.md — the runnable half of the ten review loops.

Every load-bearing property the PRD claims about itself is asserted here, so the
next edit that breaks one fails loudly instead of shipping an unverifiable doc.

    python3 docs/check-prd.py             # checks docs/PRD.md next to this script
    python3 docs/check-prd.py path.md     # or an explicit file
    python3 docs/check-prd.py --selftest  # proves the checks can actually fail
"""

import re
import sys
from pathlib import Path

# Sources the PRD is allowed to cite. A number without one of these nearby is an
# orphan assertion, which loop 1 exists to prevent.
SOURCE = re.compile(r"(Ch\.\d+|Ch\.\d+–\d+|App\. [A-G]|P\d+|design\.md|Numbers to know)")

# Bands from the book's App. B: the ledger (§20) must account for all 100.
PATTERNS = set(range(1, 101))

# Sections that carry a claim and therefore need a source in their opening prose.
SOURCED_SECTIONS = [
    "## 8.",
    "## 9.",
    "## 10.",
    "## 11.",
    "## 12.",
    "## 13.",
    "## 14.",
    "## 15.",
]

# §6 must expose the shapes a builder cannot invent.
CONTRACT_TOKENS = [
    "exit_reason",
    "tenant_id",
    "wall_clock_s",
    "Idempotency-Key",
    "Last-Event-ID",
    "**`idempotency` row**",
    "**`approval` row**",
    "**`run` row**",
    "**`step` row**",
    # both key forms, because a retry with a new run id is the incident this guards
    "run:<run_id>:tool:<tool>:args:<args_hash>",
    "caller:<idempotency_key>",
    # SSE is unbuildable without an event vocabulary
    "`state`, `step`, `approval`, `done`",
]
# Sections whose opening prose must name a source, and the token that proves it.
SOURCED_OPENINGS = {
    "## 8.": "Ch.12",
    "## 10.": "Ch.7",
    "## 11.": "Ch.10",
    "## 12.": "Ch.11",
}


def check(text: str) -> list[str]:
    fail: list[str] = []

    # 1. every internal §N reference resolves to a heading that exists.
    # Cross-document refs are stripped first: a footer line citing
    # "docs/JEV-INTEGRATION.md §3.4" points at that file's sections, not
    # this one, and counting it here was a standing false FAIL.
    # A file ref can carry a slash-chain of sections (`JEV-INTEGRATION.md`
    # §3.1/§3.4/§3.5) — the whole chain belongs to that file.
    own = re.sub(r"(?:docs/)?[A-Za-z0-9_.-]+\.md`?\s*§\s*\d+(?:\.\d+)?"
                 r"(?:\s*/\s*§\s*\d+(?:\.\d+)?)*", "", text.replace("`", ""))
    heads = {m.group(1) for m in re.finditer(r"^#{2,3} (\d+(?:\.\d+)?)\.?\s", own, re.M)}
    dangling = sorted(r for r in set(re.findall(r"§(\d+(?:\.\d+)?)", own)) if r not in heads)
    if dangling:
        fail.append(f"internal refs with no section: {dangling}")

    # 2. all 100 App. B patterns are accounted for (adopted / deferred / rejected)
    covered = {int(n) for n in re.findall(r"P(\d+)", text)}
    missing = sorted(PATTERNS - covered)
    if missing:
        fail.append(f"patterns not accounted for: {missing}")
    # An out-of-range token (P0, P101, …) is a citation that cannot point at a
    # real pattern. Reported separately: lumping it into `missing` printed an
    # empty list, which is a finding no one can act on.
    # P0 is excluded on purpose: the pattern namespace starts at P1, so a
    # "P0" in this document is a priority label (docs/PRD.md §13 defines
    # P0–P3), not a citation. Anything else outside the ledger is a typo.
    extra = sorted(n for n in covered - PATTERNS if n != 0)
    if extra:
        fail.append(f"pattern tokens outside the 1–100 ledger: {extra}")
    if "## 20. Appendix D" not in text:
        fail.append("pattern ledger (§20) is missing")

    # 3. every FR and NFR carries its why, not just its target
    frs = [ln for ln in text.splitlines() if ln.startswith("- **FR-")]
    if len(frs) != 12:
        fail.append(f"expected 12 FRs, found {len(frs)}")
    for ln in frs:
        annotation = re.search(r"\*\((.*)\)\*\s*$", ln)
        if not annotation:
            fail.append(f"FR without a trailing why-annotation: {ln[:40]}")
        elif not SOURCE.search(annotation.group(1)):
            fail.append(f"FR annotation cites no source: {ln[:40]}")
    nfrs = [ln for ln in text.splitlines() if re.match(r"\| NFR-\d ", ln)]
    if len(nfrs) != 7:
        fail.append(f"expected 7 NFRs, found {len(nfrs)}")
    for ln in nfrs:
        if ln.count("|") < 5:
            fail.append(f"NFR without a 'there because' cell: {ln[:40]}")

    # 4. the defaults table cites a real source for every row — "book prior" is not one
    if "## 17. Appendix A" not in text or "## 18. Appendix B" not in text:
        fail.append("defaults appendix (§17) or source-discount appendix (§18) missing")
    else:
        d = text[text.index("## 17. Appendix A"):text.index("## 18. Appendix B")]
        for ln in d.splitlines():
            cells = ln.split("|")
            if len(cells) >= 5 and cells[3].strip() in ("book prior", "book prior, no source"):
                fail.append(f"default with a vague source: {cells[1].strip()}")

    # 5. the two unconditional patterns are the milestone, and are named as such.
    # Anchored on the M1 row (not on any P1/P75 mention) so that deleting the
    # milestone's definition actually fails the check.
    m1_rows = [ln for ln in text.splitlines() if ln.startswith("| M1 |")]
    if not m1_rows:
        fail.append("no M1 row in the milestone table")
    elif not all(re.search(r"P1\b", ln) and re.search(r"P75\b", ln) for ln in m1_rows):
        fail.append("M1 does not name P1 Bounded Loop and P75 Kill Switch as its definition")

    # 6. no claim section opens without naming a source. Sections with a specific
    #    attribution (§8 Ch.12, §10 Ch.7, ...) must carry *that* one, so deleting
    #    the tag fails instead of being rescued by any nearby pattern id.
    for tag in SOURCED_SECTIONS:
        if tag not in text:
            fail.append(f"section missing: {tag}")
            continue
        opening = text[text.index(tag):text.index(tag) + 420]
        if not SOURCE.search(opening):
            fail.append(f"{tag} opens with no source in its first 420 chars")
    for tag, token in SOURCED_OPENINGS.items():
        if tag not in text:
            continue
        after = text[text.index(tag):].splitlines()
        # the first non-blank line of the section carries the attribution, so
        # deleting the tag cannot be rescued by an unrelated mention below
        heading, body = after[0], next((ln for ln in after[1:] if ln.strip()), "")
        # the attribution must be the section's own, so the first sentence only —
        # a mention 300 chars later is a different claim (#10 defers to §10's gate)
        if token not in heading and token not in body[:200]:
            fail.append(f"{tag} no longer attributes its claim to {token} (heading or first line)")

    # 7. pointers into design.md name sections a reader can follow
    if "design.md` §17 (build order)" not in text:
        fail.append("build-order pointer is not design.md §17")

    # 8. the API contract is concrete enough to implement against
    for token in CONTRACT_TOKENS:
        if token not in text:
            fail.append(f"§6 contract missing: {token}")
    # M3's acceptance is only testable if the *requirement* exists in §11.4,
    # not merely a mention of it in the review appendix
    if "Config changes also run the paired parity comparison" not in text:
        fail.append("the parity harness behind M3's cost acceptance is not specified")

    # 8b. the parity harness is not merely *specified* — §11.4 says where it
    # lives. Scoped to the section on purpose: the filename also appears in
    # §3.1, D9 and the footer, so an unscoped search passed while §11.4 itself
    # named no implementation (found by --selftest, which is why it exists).
    if "### 11.4" in text and "### 11.5" in text:
        s114 = text[text.index("### 11.4"):text.index("### 11.5")]
        if "internal/eval/parity.go" not in s114:
            fail.append("§11.4 specifies the parity harness but names no implementation")
    # D1 answers the language; without a row for the framework, the choice is
    # invisible to a reviewer reading §15 — which is how it went undecided.
    if not any(ln.startswith("| D9 |") for ln in text.splitlines()):
        fail.append("no D9 row: the framework-vs-custom decision has no home in §15")
    elif "design.md` §18" not in text[text.index("| D9 |"):text.index("| D9 |") + 700]:
        fail.append("D9 does not cite the chooser it comes from (design.md §18)")

    # 9. provenance for both source sweeps is recorded
    if "QC pass (2026-09-18)" not in text:
        fail.append("the claim-verification sweep is not recorded in §16")

    # 10. the document states its own weaknesses and its own summary
    for tag in ("## 22. Appendix F", "## 23. Appendix G"):
        if tag not in text:
            fail.append(f"missing appendix: {tag}")

    # 11. a document of record is stamped with when it last changed
    if "Last updated" not in text:
        fail.append("no 'Last updated' footer stamp")

    # 12. markdown hygiene: well-formed tables, balanced fences
    for i, ln in enumerate(text.splitlines(), 1):
        if ln.startswith("|") and ln.count("|") < 3:
            fail.append(f"malformed table row at line {i}")
    if text.count("```") % 2:
        fail.append("unbalanced code fences")

    return fail


# Deliberate breakages, each naming the property it must trip. A check that cannot
# fail is worse than no check, so `--selftest` asserts every one of these is caught.
MUTATIONS = [
    ("M1 loses P1/P75", "P1 Bounded Loop and P75 Kill Switch, are the milestone", "bounded loop, is the milestone"),
    ("a pattern disappears", "P63 Adversarial Check", "P9999 Adversarial Check"),
    ("an FR annotation loses its source", "*(P92 Status Updates: the book claims 10× longer waits are tolerated when progress is visible", "*(the API should stream"),
    ("the §6 run row is removed", "**`run` row** — `run_id` (ULID)", "**run columns** — `run_id` (ULID)"),
    ("the caller idempotency key is removed", "`caller:<idempotency_key>`", "the caller key"),
    ("a § reference dangles", "see §15)", "see §99)"),
    ("§10 loses its attribution", "**Single agent in v1 (Ch.7).**", "**Single agent in v1.**"),
    ("§8 loses its attribution", "## 8. Error taxonomy & recovery (Ch.12)", "## 8. Error taxonomy & recovery"),
    # Every stamp, not just the first: the PRD keeps a version list, and only
    # stripping all of them is a document without one.
    ("the footer stamp is dropped", "Last updated:", "Last change:"),
    ("the pattern ledger is renamed", "## 20. Appendix D", "## 20. Appendix Z"),
    ("the parity harness is dropped", "**Config changes also run the paired parity comparison**", "**Config changes also run a cost comparison**"),
    ("the SSE event names are dropped", "`state`, `step`, `approval`, `done`", "several event types"),
    # Anchored on the §11.4 sentence, not the filename: the filename also
    # appears in §3.1/D9/footer, so a first-occurrence replace left §11.4
    # unnamed and the check still passed — a blind spot --selftest reported.
    ("the parity implementation is unnamed", "**Implemented 2026-09-21:** `internal/eval/parity.go`", "**Implemented 2026-09-21:** the parity harness"),
    ("D9 loses its home", "| D9 |", "| DX |"),
]


def selftest(text: str) -> int:
    if check(text):
        print("selftest cannot run: the document already fails the checks")
        return 1
    blind = []
    for name, old, new in MUTATIONS:
        if old not in text:
            blind.append(f"{name} (anchor text not found — update the fixture)")
            continue
        # The footer-mutation must hit every stamp, so it is not anchored to one.
        mutated = text.replace(old, new) if name == "the footer stamp is dropped" else text.replace(old, new, 1)
        if not check(mutated):
            blind.append(name)
    if blind:
        print(f"BLIND SPOTS in the check itself ({len(blind)}):")
        for b in blind:
            print(f"  - {b}")
        return 1
    print(f"selftest OK — all {len(MUTATIONS)} deliberate breakages are caught")
    return 0


def main() -> int:
    if "--selftest" in sys.argv:
        return selftest(Path(__file__).with_name("PRD.md").read_text(encoding="utf-8"))
    target = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).with_name("PRD.md")
    text = target.read_text(encoding="utf-8")
    fail = check(text)

    lines = len(text.splitlines())
    sections = len(re.findall(r"^## ", text, re.M))
    sources = len(set(re.findall(r"Ch\.\d+|App\. [A-G]|P\d+", text)))
    print(f"{target}: {lines} lines, {sections} sections, {sources} distinct source tokens")
    if fail:
        print(f"FAIL ({len(fail)}):")
        for f in fail:
            print(f"  - {f}")
        return 1
    print("OK — all structural checks pass")
    return 0


if __name__ == "__main__":
    sys.exit(main())
