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
  await page.getByLabel("Author").fill("v1a1t1-abc123-dj");

  // step 3: fill
  await page.getByLabel("Beat name").fill("v1a1t1-abc123-slow");

  // step 4: fill
  await page.locator("css=#bpm").fill("75");

  // step 5: click
  await page.getByRole("button", { name: "Save" }).click();
  await maul.settled(page);

  // step 6: expect text
  await expect(page.locator("css=#fresh li").filter({ hasText: "v1a1t1-abc123-slow" })).toContainText("75 bpm");
  await maul.text(page.locator("css=#fresh li").filter({ hasText: "v1a1t1-abc123-slow" }), { contains: "75 bpm" });

  // step 7: fill
  await page.getByLabel("Beat name").fill("v1a1t1-abc123-fast");

  // step 8: fill
  await page.locator("css=#bpm").fill("150");

  // step 9: click
  await page.getByRole("button", { name: "Save" }).click();
  await maul.settled(page);

  // step 10: expect text
  await expect(page.locator("css=#fresh li").filter({ hasText: "v1a1t1-abc123-fast" })).toContainText("150 bpm");
  await maul.text(page.locator("css=#fresh li").filter({ hasText: "v1a1t1-abc123-fast" }), { contains: "150 bpm" });

  // step 11: goto
  await page.goto("/");
  await maul.settled(page);

  // step 12: expect text
  await expect(page.locator("css=#tempo-out")).toHaveText("medium");
  await maul.text(page.locator("css=#tempo-out"), { equals: "medium" });

  // step 13: click
  await page.locator("css=#fresh li").filter({ hasText: "v1a1t1-abc123-slow" }).locator("css=button.load").click();
  await maul.settled(page);

  // step 14: expect text
  await expect(page.locator("css=#bpm-out")).toHaveText("75");
  await maul.text(page.locator("css=#bpm-out"), { equals: "75" });

  // step 15: expect text
  await expect(page.locator("css=#tempo-out")).toHaveText("slow");
  await maul.text(page.locator("css=#tempo-out"), { equals: "slow" });

  // step 16: click
  await page.locator("css=#fresh li").filter({ hasText: "v1a1t1-abc123-fast" }).locator("css=button.load").click();
  await maul.settled(page);

  // step 17: expect text
  await expect(page.locator("css=#bpm-out")).toHaveText("150");
  await maul.text(page.locator("css=#bpm-out"), { equals: "150" });

  // step 18: expect text
  await expect(page.locator("css=#tempo-out")).toHaveText("fast");
  await maul.text(page.locator("css=#tempo-out"), { equals: "fast" });

  // step 19: fill
  await page.locator("css=#bpm").fill("110");

  // step 20: expect text
  await expect(page.locator("css=#tempo-out")).toHaveText("medium");
  await maul.text(page.locator("css=#tempo-out"), { equals: "medium" });

  // step 21: click
  await page.locator("css=#fresh li").filter({ hasText: "v1a1t1-abc123-slow" }).locator("css=button.load").click();
  await maul.settled(page);

  // step 22: expect text
  await expect(page.locator("css=#bpm-out")).toHaveText("75");
  await maul.text(page.locator("css=#bpm-out"), { equals: "75" });

  // step 23: expect text
  await expect(page.locator("css=#tempo-out")).toHaveText("slow");
  await maul.text(page.locator("css=#tempo-out"), { equals: "slow" });
});
