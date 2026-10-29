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
  await page.goto("/battles");
  await maul.settled(page);

  // step 2: fill
  await page.getByLabel("Name").fill("Battle v1a1t1-abc123");

  // step 3: press
  await page.getByLabel("Name").press("Enter");
  await maul.settled(page);

  // step 4: waitForURL
  await maul.waitForPath(page, "/battles/*");

  // step 5: expect text
  await expect(page.getByRole("heading").nth(0)).toHaveText("Battle v1a1t1-abc123");

  // step 6: expect text
  await expect(page.locator("css=article.side").getByText("Score")).toContainText("points");

  // step 7: expect text
  await expect(page.getByTestId("score-a")).toHaveText(maul.regex("^[0-9]+$"));
});
