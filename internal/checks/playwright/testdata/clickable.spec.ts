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
  await page.goto("/battles/new");
  await maul.settled(page);

  // step 2: click
  await page.getByRole("button", { name: "Start battle" }).click();
  await maul.settled(page);

  // step 3: waitForURL
  await maul.waitForPath(page, "/battles/*");

  // step 4: expect clickable
  await maul.clickable(page.getByTestId("vote-a"));
});
