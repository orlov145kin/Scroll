// SPDX-License-Identifier: MIT

pragma solidity =0.8.16;

import {PausableUpgradeable} from "@openzeppelin/contracts-upgradeable/security/PausableUpgradeable.sol";

import {IL2ScrollMessenger} from "./IL2ScrollMessenger.sol";
import {L2MessageQueue} from "./predeploys/L2MessageQueue.sol";
import {IL1BlockContainer} from "./predeploys/IL1BlockContainer.sol";
import {IL1GasPriceOracle} from "./predeploys/IL1GasPriceOracle.sol";

import {PatriciaMerkleTrieVerifier} from "../libraries/verifier/PatriciaMerkleTrieVerifier.sol";
import {ScrollConstants} from "../libraries/constants/ScrollConstants.sol";
import {AddressAliasHelper} from "../libraries/common/AddressAliasHelper.sol";
import {IScrollMessenger} from "../libraries/IScrollMessenger.sol";
import {ScrollMessengerBase} from "../libraries/ScrollMessengerBase.sol";

// solhint-disable reason-string

/// @title L2ScrollMessenger
/// @notice The `L2ScrollMessenger` contract can:
///
/// 1. send messages from layer 2 to layer 1;
/// 2. relay messages from layer 1 layer 2;
/// 3. drop expired message due to sequencer problems.
///
/// @dev It should be a predeployed contract on layer 2 and should hold infinite amount
/// of Ether (Specifically, `uint256(-1)`), which can be initialized in Genesis Block.
contract L2ScrollMessenger is ScrollMessengerBase, PausableUpgradeable, IL2ScrollMessenger {
    /**********
     * Events *
     **********/

    /// @notice Emitted when the maximum number of times each message can fail in L2 is updated.
    /// @param maxFailedExecutionTimes The new maximum number of times each message can fail in L2.
    event UpdateMaxFailedExecutionTimes(uint256 maxFailedExecutionTimes);

    /*************
     * Constants *
     *************/

    /// @notice The address of L2MessageQueue.
    address public immutable messageQueue;

    /*************
     * Variables *
     *************/

    /// @notice Mapping from L2 message hash to sent status.
    mapping(bytes32 => bool) public isL2MessageSent;

    /// @notice Mapping from L1 message hash to a boolean value indicating if the message has been successfully executed.
    mapping(bytes32 => bool) public isL1MessageExecuted;

    /// @notice Mapping from L1 message hash to the number of failure times.
    mapping(bytes32 => uint256) public l1MessageFailedTimes;

    /// @notice The maximum number of times each L1 message can fail on L2.
    uint256 public maxFailedExecutionTimes;

    /***************
     * Constructor *
     ***************/

    constructor(address _messageQueue) {
        _disableInitializers();

        messageQueue = _messageQueue;
    }

    function initialize(address _counterpart, address _feeVault) external initializer {
        PausableUpgradeable.__Pausable_init();
        ScrollMessengerBase._initialize(_counterpart, _feeVault);

        maxFailedExecutionTimes = 3;
    }

    /*****************************
     * Public Mutating Functions *
     *****************************/

    /// @inheritdoc IScrollMessenger
    function sendMessage(
        address _to,
        uint256 _value,
        bytes memory _message,
        uint256 _gasLimit
    ) external payable override whenNotPaused {
        _sendMessage(_to, _value, _message, _gasLimit);
    }

    /// @inheritdoc IScrollMessenger
    function sendMessage(
        address _to,
        uint256 _value,
        bytes calldata _message,
        uint256 _gasLimit,
        address
    ) external payable override whenNotPaused {
        _sendMessage(_to, _value, _message, _gasLimit);
    }

    /// @inheritdoc IL2ScrollMessenger
    function relayMessage(
        address _from,
        address _to,
        uint256 _value,
        uint256 _nonce,
        bytes memory _message
    ) external override whenNotPaused {
        // It is impossible to deploy a contract with the same address, reentrance is prevented in nature.
        require(AddressAliasHelper.undoL1ToL2Alias(msg.sender) == counterpart, "Caller is not L1ScrollMessenger");

        bytes32 _xDomainCalldataHash = keccak256(_encodeXDomainCalldata(_from, _to, _value, _nonce, _message));

        require(!isL1MessageExecuted[_xDomainCalldataHash], "Message was already successfully executed");

        _executeMessage(_from, _to, _value, _message, _xDomainCalldataHash);
    }

    /************************
     * Restricted Functions *
     ************************/

    /// @notice Pause the contract
    /// @dev This function can only called by contract owner.
    /// @param _status The pause status to update.
    function setPause(bool _status) external onlyOwner {
        if (_status) {
            _pause();
        } else {
            _unpause();
        }
    }

    /// @notice Update max failed execution times.
    /// @dev This function can only called by contract owner.
    /// @param _maxFailedExecutionTimes The new max failed execution times.
    function updateMaxFailedExecutionTimes(uint256 _maxFailedExecutionTimes) external onlyOwner {
        require(_maxFailedExecutionTimes > 0, "maxFailedExecutionTimes cannot be zero");

        maxFailedExecutionTimes = _maxFailedExecutionTimes;

        emit UpdateMaxFailedExecutionTimes(_maxFailedExecutionTimes);
    }

    /**********************
     * Internal Functions *
     **********************/

    /// @dev Internal function to send cross domain message.
    /// @param _to The address of account who receive the message.
    /// @param _value The amount of ether passed when call target contract.
    /// @param _message The content of the message.
    /// @param _gasLimit Optional gas limit to complete the message relay on corresponding chain.
    function _sendMessage(
        address _to,
        uint256 _value,
        bytes memory _message,
        uint256 _gasLimit
    ) internal nonReentrant {
        require(msg.value == _value, "msg.value mismatch");

        uint256 _nonce = L2MessageQueue(messageQueue).nextMessageIndex();
        bytes32 _xDomainCalldataHash = keccak256(_encodeXDomainCalldata(msg.sender, _to, _value, _nonce, _message));

        // normally this won't happen, since each message has different nonce, but just in case.
        require(!isL2MessageSent[_xDomainCalldataHash], "Duplicated message");
        isL2MessageSent[_xDomainCalldataHash] = true;

        L2MessageQueue(messageQueue).appendMessage(_xDomainCalldataHash);

        emit SentMessage(msg.sender, _to, _value, _nonce, _gasLimit, _message);
    }

    /// @dev Internal function to execute a L1 => L2 message.
    /// @param _from The address of the sender of the message.
    /// @param _to The address of the recipient of the message.
    /// @param _value The msg.value passed to the message call.
    /// @param _message The content of the message.
    /// @param _xDomainCalldataHash The hash of the message.
    function _executeMessage(
        address _from,
        address _to,
        uint256 _value,
        bytes memory _message,
        bytes32 _xDomainCalldataHash
    ) internal {
        // @note check more `_to` address to avoid attack in the future when we add more gateways.
        require(_to != messageQueue, "Forbid to call message queue");
        require(_to != address(this), "Forbid to call self");

        // @note This usually will never happen, just in case.
        require(_from != xDomainMessageSender, "Invalid message sender");

        xDomainMessageSender = _from;
        // solhint-disable-next-line avoid-low-level-calls
        (bool success, ) = _to.call{value: _value}(_message);
        // reset value to refund gas.
        xDomainMessageSender = ScrollConstants.DEFAULT_XDOMAIN_MESSAGE_SENDER;

        if (success) {
            isL1MessageExecuted[_xDomainCalldataHash] = true;
            emit RelayedMessage(_xDomainCalldataHash);
        } else {
            unchecked {
                uint256 _failedTimes = l1MessageFailedTimes[_xDomainCalldataHash] + 1;
                require(_failedTimes <= maxFailedExecutionTimes, "Exceed maximum failure times");
                l1MessageFailedTimes[_xDomainCalldataHash] = _failedTimes;
            }
            emit FailedRelayedMessage(_xDomainCalldataHash);
        }
    }
}
