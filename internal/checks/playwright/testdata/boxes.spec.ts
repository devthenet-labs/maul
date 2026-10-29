// Compiled by maul from a saved browser check (internal/checks/playwright).
// Do not edit: maul runs only what it compiles. Agent text appears only in
// string literals. Viewport 1280x800, action timeout 5000 ms.
import { test } from "./maul-fixture";
import { maul } from "./maul-checks";

test.use({ viewport: { width: 1280, height: 800 }, actionTimeout: 5000 });

test("maul check", async ({ page }) => {
  test.setTimeout(30000);
  await page.setViewportSize({ width: 1280, height: 800 });

  // step 1: goto
  await page.goto("/battles/v1a1t1-abc123");
  await maul.settled(page);

  // step 2: expect left-of
  await maul.leftOf(page.locator("css=.side").nth(0), page.locator("css=.side").nth(1), 2);

  // step 3: expect above
  await maul.above(page.getByRole("heading", { name: "Battle" }), page.getByTestId("versus"), 0);

  // step 4: expect no-overlap
  await maul.noOverlap(page.getByTestId("side-a"), page.getByTestId("side-b"), 0);

  // step 5: expect equal-size
  await maul.equalSize(page.getByTestId("side-a"), page.getByTestId("side-b"), 1.5);

  // step 6: expect size
  await maul.size(page.getByTestId("side-a"), { width: 400 }, 40);

  // step 7: expect size
  await maul.size(page.getByTestId("side-b"), { height: 220.5 }, 0);
});
