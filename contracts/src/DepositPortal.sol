// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

/// @title DepositPortal
/// @notice L1 entry point of the go-mini-rollup. Anyone can lock ETH here; the rollup's derivation
/// reads the `Deposit` events of each L1 block and mints the same amount on L2 as a forced
/// (censorship-resistant) transaction: the sequencer cannot skip or reorder it.
/// @dev Deliberately minimal and WITHOUT withdrawals: the locked ETH cannot leave. Withdrawals,
/// proofs and challenges live in the `rollup-bridge` project. Not audited. Do not use with value.
// Reason: locked-ether is the documented design of this lab contract. Deposits are mirrored on L2 and
// the withdrawal path (proofs, challenge period) is out of scope here; it lives in the rollup-bridge
// project. The limitation is stated in the contract natspec, in README.md and in SECURITY.md.
// slither-disable-next-line locked-ether
contract DepositPortal {
    /// @notice Emitted for every deposit. `depositId` is a global, gap-free counter.
    event Deposit(address indexed from, address indexed to, uint256 value, uint256 depositId);

    error ZeroValue();
    error ZeroRecipient();
    error DepositTooLarge(uint256 value, uint256 max);

    /// @notice Per-deposit cap, a safety limit for a lab contract.
    uint256 public constant MAX_DEPOSIT = 1_000 ether;

    /// @notice Id the next deposit will get; equals the number of deposits so far.
    uint256 public nextDepositId;

    /// @notice Deposit `msg.value` for `to` on L2.
    function deposit(address to) external payable {
        _deposit(msg.sender, to);
    }

    /// @notice Plain ETH transfers deposit for the sender.
    receive() external payable {
        _deposit(msg.sender, msg.sender);
    }

    function _deposit(address from, address to) private {
        if (msg.value == 0) revert ZeroValue();
        if (to == address(0)) revert ZeroRecipient();
        if (msg.value > MAX_DEPOSIT) revert DepositTooLarge(msg.value, MAX_DEPOSIT);
        uint256 id = nextDepositId++;
        emit Deposit(from, to, msg.value, id);
    }
}
