# Evaluation corpus `eval-2026-10`

247 labelled URLs (`commerce.json` 61, `marketing.json` 66, `negatives.json` 60,
`social.json` 60) and `adjudicated.json`, the adjudication record of the 119
items on which the evaluated cleaners disagreed. Each item has an `input`, the
`expected` clean URL, optional `alt_expected` URLs that are also acceptable,
and the names it labels `tracking_params` and `functional_params`.

It is the input of the `rules-golden` (b) gate (`TestRules/Golden`): under
`AffiliateKeep`, an item is **broken** iff its output is in neither `expected`
nor `alt_expected`, and either `expected == input` and the output differs from
the input, or a `functional_params` name present in the post-unwrap URL is
missing from the output. Every broken item must be listed in
`known-broken.txt` (`<id> # <reason>`, human-owned, starts empty).

## Disclosure

- The corpus was model-generated and model-adjudicated, with no human review.
- Items were selected to **separate engines**, not to represent traffic.
- 20 items use synthetic hosts. Marketing is 27% of it.
- Some tracker names were enumerated by authors reading ClearURLs' provider
  lists (TikTok `u_code`/`preview_pb`, Etsy `organic_search_click`, eBay
  `_trkparms`/`hash`, the Walmart `ath*` family). Corpus items are not
  independent evidence for those names.
- Relabels made by this project: `commerce-003`, relabelled for MD-3 (Amazon
  `linkCode`/`linkId` are affiliate parameters, kept under the default):
  `alt_expected` gained
  `https://www.amazon.com/dp/B0BQ2N5NNQ?linkCode=ll1&tag=espressoaf-20&linkId=3f9c2b7e8a1d4c6e5b0a9f8e7d6c5b4a&language=en_US`.
  The files are otherwise byte-identical to the evaluation's.
- It is a regression suite only. Accuracy claims come from a held-out sample,
  not from this corpus.

Identifiers in the URLs are synthetic.
