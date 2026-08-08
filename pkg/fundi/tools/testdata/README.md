Captured 2026-08-08 from https://lite.duckduckgo.com/lite/?q=golang+example.
This endpoint is unofficial and the response format will drift — when the parser
breaks, recapture by hand with:

  curl -s -L \
    -H "User-Agent: Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36" \
    -H "Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8" \
    -H "Accept-Language: en-US,en;q=0.9" \
    -H "Accept-Encoding: identity" \
    "https://lite.duckduckgo.com/lite/?q=golang+example" > pkg/fundi/tools/testdata/ddg_lite_response.html

Then run the tests to verify the parser still works.
