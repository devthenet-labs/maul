// Compiled by maul from a saved browser check (internal/checks/playwright).
// Do not edit: maul runs only what it compiles. Agent text appears only in
// string literals. Viewport 1280x800, action timeout 5000 ms.
import { test, expect } from "./maul-fixture";
import { maul } from "./maul-checks";

test.use({ viewport: { width: 1280, height: 800 }, actionTimeout: 5000 });

test("maul check", async ({ page }) => {
  test.setTimeout(30000);
  await page.setViewportSize({ width: 1280, height: 800 });

  // step 1: goto
  await page.goto("/");
  await maul.settled(page);

  // step 2: fill
  await page.getByLabel("Beat name").fill("v1a1t1-abc123 cover");

  // step 3: fill
  await page.getByLabel("Author").fill("v1a1t1-abc123-cv");

  // step 4: click
  await page.getByRole("button", { name: "Save" }).click();
  await maul.settled(page);

  // step 5: expect visible
  await maul.visible(page.getByRole("button", { name: "Vote for v1a1t1-abc123 cover" }));

  // step 6: goto
  await page.goto("/");
  await maul.settled(page);

  // step 7: click
  await page.getByRole("region", { name: "Fresh beats" }).getByRole("button", { name: "v1a1t1-abc123 cover" }).nth(0).click();
  await maul.settled(page);

  // step 8: expect text
  await expect(page.locator("css=#loaded")).toHaveText("Loaded: v1a1t1-abc123 cover by v1a1t1-abc123-cv");

  // step 9: expect visible
  await maul.visible(page.locator("css=#loaded"));

  // step 10: expect legible
  await maul.legible(page.locator("css=#loaded"));

  // step 11: expect visible
  await maul.visible(page.locator("css=[aria-label=\"kick step 1\"]"));

  // step 12: expect visible
  await maul.visible(page.getByRole("button", { name: "Play" }));

  // step 13: expect visible
  await maul.visible(page.getByRole("button", { name: "Stop" }));

  // step 14: expect clickable
  await maul.clickable(page.getByRole("button", { name: "Play" }));

  // step 15: expect clickable
  await maul.clickable(page.getByRole("button", { name: "Stop" }));

  // step 16: expect no-overlap
  await maul.noOverlap(page.locator("css=#loaded"), page.getByRole("button", { name: "Play" }), 0);

  // step 17: expect no-overlap
  await maul.noOverlap(page.locator("css=#loaded"), page.getByRole("button", { name: "Stop" }), 0);

  // step 18: expect clickable
  await maul.clickable(page.locator("css=[aria-label=\"kick step 1\"]"));

  // step 19: expect no-overlap
  await maul.noOverlap(page.locator("css=#loaded"), page.locator("css=[aria-label=\"kick step 1\"]"), 0);
});
