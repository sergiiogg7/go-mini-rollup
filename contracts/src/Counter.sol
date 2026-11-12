// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

/// @notice Tiny contract used by the demo to show that L2 runs a real EVM.
contract Counter {
    uint256 public number;

    event Incremented(address indexed by, uint256 newValue);

    function increment() external {
        number += 1;
        emit Incremented(msg.sender, number);
    }
}
