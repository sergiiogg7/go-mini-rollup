// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test} from "forge-std/Test.sol";
import {DepositPortal} from "../src/DepositPortal.sol";

contract DepositPortalTest is Test {
    event Deposit(address indexed from, address indexed to, uint256 value, uint256 depositId);

    DepositPortal portal;
    address alice = makeAddr("alice");
    address bob = makeAddr("bob");

    function setUp() public {
        portal = new DepositPortal();
        vm.deal(alice, 10_000 ether);
    }

    function test_DepositEmitsEventWithSequentialIds() public {
        vm.startPrank(alice);
        vm.expectEmit(true, true, false, true);
        emit Deposit(alice, bob, 1 ether, 0);
        portal.deposit{value: 1 ether}(bob);
        vm.expectEmit(true, true, false, true);
        emit Deposit(alice, alice, 2 ether, 1);
        (bool ok,) = address(portal).call{value: 2 ether}("");
        assertTrue(ok);
        vm.stopPrank();
        assertEq(portal.nextDepositId(), 2);
        assertEq(address(portal).balance, 3 ether);
    }

    function test_RevertWhen_ZeroValue() public {
        vm.prank(alice);
        vm.expectRevert(DepositPortal.ZeroValue.selector);
        portal.deposit(bob);
    }

    function test_RevertWhen_ZeroRecipient() public {
        vm.prank(alice);
        vm.expectRevert(DepositPortal.ZeroRecipient.selector);
        portal.deposit{value: 1}(address(0));
    }

    function test_RevertWhen_AboveCap() public {
        uint256 max = portal.MAX_DEPOSIT();
        vm.prank(alice);
        vm.expectRevert(
            abi.encodeWithSelector(DepositPortal.DepositTooLarge.selector, max + 1, max)
        );
        portal.deposit{value: max + 1}(bob);
    }

    function testFuzz_BalanceAndCounterTrackDeposits(uint96[] calldata amounts) public {
        uint256 total;
        uint256 count;
        for (uint256 i = 0; i < amounts.length && i < 20; i++) {
            uint256 v = bound(amounts[i], 1, portal.MAX_DEPOSIT());
            vm.prank(alice);
            portal.deposit{value: v}(bob);
            total += v;
            count++;
        }
        assertEq(address(portal).balance, total);
        assertEq(portal.nextDepositId(), count);
    }
}
