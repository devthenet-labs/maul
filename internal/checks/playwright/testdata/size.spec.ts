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
  await page.goto("/sequencer");
  await maul.settled(page);

  // step 2: expect size
  await maul.size(page.getByTestId("kick-pad"), { width: 48, height: 48 }, 0.5);

  // step 3: expect size
  await maul.size(page.getByTestId("snare-pad"), { minWidth: 48, minHeight: 48 }, 0);

  // step 4: expect size
  await maul.size(page.getByTestId("grid"), { maxWidth: 1200, minHeight: 200, maxHeight: 400 }, 1);
});
