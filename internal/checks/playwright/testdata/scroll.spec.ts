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

  // step 2: select
  await page.getByLabel("Tempo").selectOption({ label: "120 bpm" });

  // step 3: scroll
  await maul.scroll(page.getByRole("grid", { name: "Steps" }), 0, 40);

  // step 4: expect aligned
  await maul.aligned(page.getByRole("row", { name: "Kick v1a1t1-abc123" }), page.getByRole("row", { name: "Snare v1a1t1-abc123" }), "x", 1);
});
