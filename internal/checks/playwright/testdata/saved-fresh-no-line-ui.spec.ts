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
  await page.getByLabel("Beat name").fill("v1a1t1-abc123 fresh");

  // step 3: fill
  await page.getByLabel("Author").fill("v1a1t1-abc123-fr");

  // step 4: click
  await page.getByRole("button", { name: "Save" }).click();
  await maul.settled(page);

  // step 5: expect visible
  await maul.visible(page.getByRole("button", { name: "Vote for v1a1t1-abc123 fresh" }));

  // step 6: goto
  await page.goto("/");
  await maul.settled(page);

  // step 7: expect visible
  await maul.visible(page.getByRole("button", { name: "Vote for v1a1t1-abc123 fresh" }));

  // step 8: expect text
  await expect(page.getByRole("region", { name: "Sequencer" })).toHaveText(maul.regex("^\\s*(?i:kick)"));
  await maul.text(page.getByRole("region", { name: "Sequencer" }), { matches: "^\\s*(?i:kick)" });

  // step 9: expect count
  await expect(page.getByText("Loaded:").filter({ hasText: "v1a1t1-abc123" })).toHaveCount(0);

  // step 10: click
  await page.getByRole("region", { name: "Fresh beats" }).getByRole("button", { name: "v1a1t1-abc123 fresh" }).nth(0).click();
  await maul.settled(page);

  // step 11: expect text
  await expect(page.getByText("Loaded:")).toHaveText("Loaded: v1a1t1-abc123 fresh by v1a1t1-abc123-fr");
  await maul.text(page.getByText("Loaded:"), { equals: "Loaded: v1a1t1-abc123 fresh by v1a1t1-abc123-fr" });

  // step 12: expect text
  await expect(page.getByRole("region", { name: "Sequencer" })).toHaveText(maul.regex("^\\s*Loaded: "));
  await maul.text(page.getByRole("region", { name: "Sequencer" }), { matches: "^\\s*Loaded: " });

  // step 13: goto
  await page.goto("/");
  await maul.settled(page);

  // step 14: expect text
  await expect(page.getByRole("region", { name: "Sequencer" })).toHaveText(maul.regex("^\\s*(?i:kick)"));
  await maul.text(page.getByRole("region", { name: "Sequencer" }), { matches: "^\\s*(?i:kick)" });

  // step 15: expect count
  await expect(page.getByText("Loaded:").filter({ hasText: "v1a1t1-abc123" })).toHaveCount(0);
});
