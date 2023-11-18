use crate::{legacy::instruction::InitializeArgs, state::Config};
use anchor_lang::prelude::*;
use core_bridge_program::sdk as core_bridge;

#[derive(Accounts)]
pub struct Initialize<'info> {
    #[account(mut)]
    payer: Signer<'info>,

    #[account(
        init,
        payer = payer,
        space = Config::INIT_SPACE,
        seeds = [Config::SEED_PREFIX],
        bump,
    )]
    config: Account<'info, core_bridge::legacy::LegacyAnchorized<Config>>,

    /// Previously needed sysvar.
    ///
    /// CHECK: This account is unchecked.
    _rent: UncheckedAccount<'info>,

    system_program: Program<'info, System>,
}

impl<'info> core_bridge::legacy::ProcessLegacyInstruction<'info, InitializeArgs>
    for Initialize<'info>
{
    const LOG_IX_NAME: &'static str = "LegacyInitialize";

    const ANCHOR_IX_FN: fn(Context<Self>, InitializeArgs) -> Result<()> = initialize;
}

fn initialize(ctx: Context<Initialize>, _args: InitializeArgs) -> Result<()> {
    // NOTE: This config account is pointless and is never used in any of the instruction handlers.
    ctx.accounts.config.set_inner(
        Config {
            core_bridge_program: core_bridge_program::ID,
        }
        .into(),
    );

    // Done.
    Ok(())
}
