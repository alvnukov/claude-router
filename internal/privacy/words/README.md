# Embedded collision dictionaries

Downloaded/extracted 2026-09-27. UTF-8, one lower-case entry per line, sorted
and deduplicated; gzip embedded into the binary. Used only to reject invented
pseudonyms, not as an automatic person recognizer. Runtime membership uses a
shared Bloom filter: false positives cause regeneration; no false negatives.

| File | Entries | Source / license |
|---|---:|---|
| surnames_en.txt.gz | 162253 | US Census 2010, public domain |
| words_en.txt.gz | 124351 | SCOWL 2020.12.07, size 60, en_US + en_GB incl. british_z, english and special lists; SCOWL-LICENSE.txt |
| words_ru.txt.gz | 3064812 | OpenCorpora 0.92 revision 417150, all unique dictionary word forms; CC BY-SA 3.0, OPENCORPORA-LICENSE.txt |

Census source: https://www.census.gov/topics/population/genealogy/data/2010_surnames.html
The Census download endpoint returned 403. Used the same dataset distributed
by CFPB: https://github.com/cfpb/proxy-methodology/blob/master/input_files/Names_2010Census.csv
Extracted the name column, excluding the aggregate ALL OTHER NAMES row.

SCOWL release: https://sourceforge.net/projects/wordlist/files/SCOWL/2020.12.07/
Combined the English/US/British word lists up to size 60, including names,
then lowercased, sorted and deduplicated. See bundled full copyright notices.

OpenCorpora: https://opencorpora.org/dict.php
Direct downloads returned 521. Extracted the compiled dictionary distributed
by pymorphy3-dicts-ru (via pymorphy3 2.0.6); build metadata is preserved in
opencorpora-source.json. The source has 391778 lexemes. This is intentionally
**all word forms**, a superset of lemmas, rather than accidentally omitting names
by filtering grammar tags. The larger compressed file is about 8.7 MiB. The P1
plan incorrectly called the license CC BY-SA 4.0: the actual source license is
CC BY-SA 3.0. Attribution: OpenCorpora contributors. Changes: extraction,
lowercasing, sorting, deduplication and compression; this derived list remains
under CC BY-SA 3.0. The license does not change the license of unrelated code.

SHA-256 of compressed files:

```
c2c8feea3bc3c8e49ed89a1077f3ed0ab377b096ac5324267d7ebc13ddb4e102  surnames_en.txt.gz
87face0f44e484574398eea3177a12cb4468f8d78888f4c9bbc9c3fb78a28543  words_en.txt.gz
ef513a023ccdcf66a644d5335d2947f9b6a43e916b2fb1a8d2a4e080758dd795  words_ru.txt.gz
```

Secret-prefix patterns were researched against Gitleaks v8.18.4, MIT license
in ../GITLEAKS-LICENSE. Upstream config:
https://github.com/gitleaks/gitleaks/blob/v8.18.4/config/gitleaks.toml
SHA-256: b94aad1d0f105e4d7cb230872461705a4b04a7743c656836144ae6853cb33e1b.
The filter uses its own bounded subset and context rules in ../defaults.json;
it does not claim full Gitleaks coverage.
