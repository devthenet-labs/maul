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

  // step 2: fill
  await page.getByLabel("Beat name", { exact: true }).fill("v1a1t1-abc123 a");

  // step 3: select
  await page.getByLabel("Tempo", { exact: true }).selectOption({ label: "120 bpm" });

  // step 4: click
  await page.getByRole("region", { name: "Save a beat" }).getByRole("button", { name: "Save", exact: true }).click();
  await maul.settled(page);

  // step 5: expect visible
  await maul.visible(page.getByRole("button", { name: "Vote for v1a1t1-abc123 a", exact: true }));
});
