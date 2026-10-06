# Station catalogue (`stations.json`)

Hand-curated reference data for each numbers station the site can list: its Priyom page, language, attributed operator, transmitter site and a typical transmission length. The schedule feed only gives start times, so the app uses `typicalDurationMin` to decide how long a transmission is "on now" (live signal detection can end it early).

**Status: machine-seeded on 2026-10-06, needs human review.** Every entry was written by an agent from the Priyom pages fetched that day. Nobody has checked it by hand yet.

## Source and licence

All station information is derived from the station pages at [Priyom.org](https://priyom.org/number-stations), which are published under [CC BY-NC-SA 4.0](https://creativecommons.org/licenses/by-nc-sa/4.0/). This file is an adaptation of that material and is shared under the same licence. Credit Priyom.org and link to each station's `priyomUrl` wherever this data is shown. Notes are short paraphrases, not copied text. Non-commercial use only.

## Schema

The file is one JSON object. The `_meta` key holds provenance:

```json
"_meta": { "source": "Priyom.org station pages", "licence": "CC BY-NC-SA 4.0",
           "retrieved": "2026-10-06", "reviewStatus": "machine-seeded, needs human review" }
```

Every other key is a Priyom (ENIGMA) designator such as `E11`, `V13` or `XPA2`. Each value has these fields:

| Field | Meaning |
|---|---|
| `name` | Priyom's nickname for the station (for example "Oblique"), or `""` if it has none. |
| `priyomUrl` | The station's Priyom page. |
| `category` | Which Priyom index lists it: `english`, `german`, `slavic`, `other`, `morse` or `digital`. (`operators` is allowed by the schema but no entry uses it yet.) |
| `language` | The voice language, `Morse`, or `Digital (...)` with the modulation. |
| `operator` | The operator or country as Priyom attributes it, for example "Russian 7 (Russia)". |
| `txSite` | `{lat, lon, label, confidence}`. `confidence` is `known` when the page gives exact coordinates, `approximate` when it gives only a city or country (city centre or country centroid used), `unknown` with null lat/lon if nothing is stated. An optional `alternates` array lists further sites when the page names several and the schedule decides which one is used. |
| `typicalDurationMin` | Typical length in minutes of one feed entry, or `null` if there is no basis for a figure. |
| `durationSource` | `page` if the figure comes from something the page states, `estimate` if it was worked out from the message format and the repeat spacing. |
| `notes` | One short, neutral line. Plain ASCII. |

## Variant designators

The feed sometimes uses a variant suffix (`F03j`, `P03i`, `S11a`, `F06a`). A variant has its own entry only when Priyom gives it its own page (`S11a`, `S06c`, `F06a`, `M01a`, `V07f`, `XPA2`). Otherwise the app should drop the trailing lowercase letters and use the base entry: `F03j` and `F03l` use `F03`, and `P03g`, `P03i`, `P03k` and `P03l` use `P03`. All designators in the 2026-10-06 fixture and in the live feed for 6 and 7 October resolve this way.

## Coverage

There are 34 entries:

- 24 active stations from the Priyom index pages. These cover every designator in the feed.
- 10 stations Priyom lists as hibernated (E06, E25, G06, G07, S07, S25, V06, V07f, V23, HM01). They are included because they still turn up in tests. Each one's note says it is hibernated.

Inactive (historical) stations are left out.

## Things to review

- **Durations are mostly estimates.** Priyom rarely states how long a transmission lasts. The figures marked `page` are V32 (2 hours), F01 and F06 (about a 7 minute loop), S06c (4 minutes), M23 (at least 10 minutes) and HM01 (3 + 20 minutes). The rest come from the intro length, message size and the spacing between repeats. V13 is set to 55 minutes because the feed lists only the on-the-hour start, and the broadcast repeats on the half hour. F03 and P03 are set to 10 minutes because one feed entry seems to cover both transmissions, which are 5 minutes apart. Check both of these assumptions.
- **No duration given:** V28 (live operators, no stated length) and M01a (no fixed format) have `typicalDurationMin: null`, so the app's 10 minute default applies.
- **Russia with no site named:** the pages for M01, M01a, P07, V06, V07f and V23 say only "Russia". These use Moscow as a placeholder with `approximate` confidence, not the Russian centroid, which is deep in Siberia. Most sites this operator family uses are near Moscow, but the pages do not say so for these stations.
- **More than one site:** Russian 6 stations (S06, E06, F01, F06, F06a, S06c) and M12 transmit from several sites depending on the schedule. The feed's "[Target: Pacific]" entries probably come from the Far East sites (Chita, Khabarovsk), not Moscow. The main `txSite` is the first site the page lists. The others are in `alternates`.
- **E06:** the English index lists it as hibernated, but its own page says active.
- **F03 / P03:** these have no station pages of their own. `/number-stations/digital/f03` and `/digital/p03` redirect to the shared Polish 11 page `/number-stations/operators/polish-11/digital-modes`, so both entries use that URL.
- **Unresolved stations:** none. Every designator in the feed has an entry.
