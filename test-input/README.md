# Sample inputs

Synthetic fixtures used by the CI smoke tests
(`.github/workflows/test-classification.yml`) and handy for a first local run.
They exist to exercise **input-format detection**, not to be a realistic
inventory list.

| File | Shape | Exercises |
|---|---|---|
| `url_master.csv` | `Domain,Quality,Justification,Vertical,Description` | website detection and the crawl ladder |
| `app_list_master.csv` | an SSP-style app report; the identifier lives in `pageURL` | iOS numeric App Store IDs vs. Android reverse-DNS packages |
| `CTV_Master.csv` | `SSP,Publisher,"App, Account or Network Name",Bundle ID` | CTV auto-detection and bundle-ID platform inference (Roku, Fire TV, Android TV, Apple TV, Vizio, Samsung TV, Xbox) |

What is and is not real:

- **Website domains** are long-lived public sites (`example.com`, standards
  bodies, open-source projects). They resolve, so a smoke run produces real
  classifications.
- **App names, publisher names, SSP names, bundle IDs, publisher IDs, request
  counts and eCPMs are invented.** Anything that looks like commercial supply
  data is placeholder text — `Example`, `Contoso`, `Northwind` and friends.
- No customer, partner or client data appears in this repository.

Point a run at one with `--input-csv`:

```bash
python main.py --input-csv test-input/url_master.csv --scrape-mode direct
```
