import streamDeck from "@elgato/streamdeck";

import { AccountUsageAction } from "./actions/account-usage";
import { AttentionAction } from "./actions/attention";
import { FleetHealthAction } from "./actions/fleet-health";
import { GoalSessionAction } from "./actions/goal-session";
import { MasterFocusAction, MasterPromptAction } from "./actions/master";
import { PauseToggleAction } from "./actions/pause-toggle";
import { flowbee } from "./flowbee/service";
import type { GlobalSettings } from "./settings";

streamDeck.logger.setLevel("debug");

streamDeck.actions.registerAction(new AccountUsageAction());
streamDeck.actions.registerAction(new GoalSessionAction());
streamDeck.actions.registerAction(new MasterFocusAction());
streamDeck.actions.registerAction(new MasterPromptAction());
streamDeck.actions.registerAction(new PauseToggleAction());
streamDeck.actions.registerAction(new AttentionAction());
streamDeck.actions.registerAction(new FleetHealthAction());

// re-wire the shared service whenever a property inspector edits the globals.
streamDeck.settings.onDidReceiveGlobalSettings<GlobalSettings>((ev) => flowbee.configure(ev.settings));

await streamDeck.connect();
flowbee.configure(await streamDeck.settings.getGlobalSettings<GlobalSettings>());
