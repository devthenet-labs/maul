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
  await page.goto("/");
  await maul.settled(page);

  // step 2: expect legible
  await maul.legible(page.getByRole("link", { name: "Back to the sequencer" }));
});
